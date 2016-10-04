package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lomik/elock"
)

const APP = "elock"
const VERSION = "0.1"

type Config struct {
	EtcdEndpoints []string `json:"etcd-endpoints"`
	EtcdRoot      string   `json:"etcd-root"`
	EtcdTTL       string   `json:"etcd-default-ttl"`
	EtcdRefresh   string   `json:"etcd-deafult-refresh"`
}

func main() {
	configFile := flag.String("config", fmt.Sprintf("/usr/local/etc/%s.json", APP), "Config file in json format")
	printConfig := flag.Bool("config-print-default", false, "Print default config")
	nowait := flag.Bool("n", false, "Fail rather than wait")
	slots := flag.Int("s", 1, "Available slots count for lock")
	timeout := flag.Duration("w", 0, "Wait for a limited amount of time")
	version := flag.Bool("V", false, "Display version")
	debug := flag.Bool("debug", false, "Enable debug log")
	ttl := flag.Duration("ttl", 0, "Lock ttl (default from config)")
	refresh := flag.Duration("refresh", 0, "Refresh interval (default from config)")
	minLockTime := flag.Duration("min", 0, "Minimum lock time")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `%s %s
Usage: %s [options] etcd_key command

`, APP, VERSION, APP)
		flag.PrintDefaults()
	}

	flag.Parse()

	if *version {
		fmt.Println(APP, VERSION)
		os.Exit(2)
	}

	config := &Config{
		EtcdEndpoints: []string{"http://localhost:2379"},
		EtcdRoot:      "/elock",
		EtcdTTL:       "1m",
		EtcdRefresh:   "10s",
	}

	if *printConfig {
		b, _ := json.MarshalIndent(config, "", "\t")
		fmt.Println(string(b))
		os.Exit(1)
	}

	cfgData, err := ioutil.ReadFile(*configFile)
	if err != nil {
		log.Fatal(err)
	}

	err = json.Unmarshal(cfgData, config)
	if err != nil {
		log.Fatal(err)
	}

	lockTLL, err := time.ParseDuration(config.EtcdTTL)
	if err != nil {
		log.Fatal(err)
	}

	lockRefresh, err := time.ParseDuration(config.EtcdRefresh)
	if err != nil {
		log.Fatal(err)
	}

	if *ttl != 0 {
		lockTLL = *ttl
	}

	if *refresh != 0 {
		lockRefresh = *refresh
	}

	args := flag.Args()

	if len(args) < 2 || *slots < 1 {
		flag.Usage()
		return
	}

	x, err := elock.New(elock.Options{
		EtcdEndpoints: config.EtcdEndpoints,
		Path:          filepath.Join(config.EtcdRoot, args[0]),
		Slots:         *slots,
		TTL:           lockTLL,
		Refresh:       lockRefresh,
		Debug:         *debug,
		MinLockTime:   *minLockTime,
	})

	if err != nil {
		log.Fatal(err)
	}

	if *nowait {
		err = x.LockNoWait()
	} else if *timeout != 0 {
		err = x.LockTimeout(*timeout)
	} else {
		err = x.Lock()
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGSTOP, syscall.SIGTERM)

	go func() {
		for _ = range c {
			// sig is a ^C, handle it
			x.Unlock()
			os.Exit(1)
		}
	}()

	if err != nil {
		log.Fatal(err)
	}

	cmd := exec.Command(args[1], args[2:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	err = cmd.Run()

	x.Unlock()

	if err != nil {
		log.Fatal(err)
	}
}