package main

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	cfenv "github.com/cloudfoundry-community/go-cfenv"
	"github.com/garyburd/redigo/redis"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	redsync "gopkg.in/redsync.v1"
)

func main() {
	//create and register prometheus metric.
	tick := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tick_counter_minutes",
		Help: "heartbeat",
	})

	prometheus.MustRegister(tick)

	//Get Cloud Foundry environment variables
	cfEnv, err := cfenv.Current()
	if err != nil {
		panic(err)
	}

	//Find the redis credentials
	redisServices, err := cfEnv.Services.WithTag("redis")
	if err != nil {
		panic(err)
	}
	redisCreds := redisServices[0].Credentials

	//Create redis connection pool and pubsub client
	redisPools := []redsync.Pool{redis.NewPool(func() (redis.Conn, error) {
		c, redisErr := redis.Dial("tcp", redisCreds["hostname"].(string)+":"+redisCreds["port"].(string), redis.DialPassword(redisCreds["password"].(string)))
		if redisErr != nil {
			return nil, redisErr
		}

		return c, redisErr
	}, 3)}

	ps := redis.PubSubConn{Conn: redisPools[0].Get()}

	//Create mutex
	m := redsync.New(redisPools).NewMutex("tick", redsync.SetRetryDelay(1*time.Second), redsync.SetExpiry(60*time.Second))

	//go routine tries to get a lock and increase the counter. IF it succesfully increases the counter it publishes the new value through redis pubsub
	go func() {
		conn := redisPools[0].Get()
		defer conn.Close()

		ps.Subscribe("counter-updated")
		defer ps.Unsubscribe("counter-updated")

		for {
			m.Lock()
			fmt.Println("Acquired Lock.")

			count, incrErr := redis.Int64(conn.Do("INCR", "counter"))
			if incrErr != nil {
				fmt.Println("Could not increase counter value. Unlocking so somebody else can try." + incrErr.Error())
				m.Unlock()
				time.Sleep(10 * time.Second) //Sleep to rate limit the retries in case no other node succesfully increases the counter
			} else {
				fmt.Printf("Published counter value: %v\n", count)
				conn.Do("PUBLISH", "counter-updated", count)
			}
		}
	}()

	//pubsub receiver. Gets the counter value via redis pubsub and updates the prometheus metric
	go func() {
		var counter int64
		for {
			switch message := ps.Receive().(type) {
			case redis.Message:
				counter, _ = strconv.ParseInt(string(message.Data), 10, 64)
				fmt.Printf("Received counter value: %v\n", counter)
				tick.Set(float64(counter))
			}
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	err = http.ListenAndServe(fmt.Sprintf(":%v", cfEnv.Port), nil)
	if err != nil {
		panic(err)
	}
}
