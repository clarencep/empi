package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

const SIGUSR1 = syscall.Signal(10)
const SIGUSR2 = syscall.Signal(12)

func main() {
	maxRunTime := 0.0
	flag.Float64Var(&maxRunTime, "t", 10, "max run time")
	flag.Parse()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Kill, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, SIGUSR1, SIGUSR2, syscall.SIGABRT)

	startTime := time.Now()
	log.Printf("#test-run# test-run is started!")

	for {
		select {
		case s := <-signals:
			log.Printf("#test-run# Got signal: %v", s)
			if s == os.Kill {
				log.Printf("#test-run# Killed -> 1.")
				os.Exit(1)
			} else if s == os.Interrupt {
				log.Printf("#test-run# Interrupt -> 2.")
				os.Exit(2)
			} else if s == syscall.SIGTERM || s == syscall.SIGHUP || s == syscall.SIGABRT {
				log.Printf("#test-run# Terminated. -> 3.")
				os.Exit(3)
			} else if s == syscall.SIGQUIT {
				log.Printf("#test-run# will quit after 1 seconds")
				<-time.After(1 * time.Second)
				log.Printf("#test-run# quit. -> 4")
				os.Exit(4)
			} else {
				log.Printf("#test-run# Unknown signal: %v", s)
			}
		case <-time.After(1000 * time.Millisecond):
			runTime := time.Now().Sub(startTime)
			log.Printf("#test-run# Run time: %s", runTime)
			if runTime.Seconds() >= maxRunTime {
				log.Printf("#test-run# timeout, exit -> 0.")
				os.Exit(0)
			}
		}

		runtime.Gosched()
	}
}
