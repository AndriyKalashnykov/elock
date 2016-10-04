package elock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lomik/elock/etcd"
)

type Options struct {
	EtcdEndpoints []string

	Path        string
	Slots       int
	TTL         time.Duration
	Refresh     time.Duration
	Debug       bool
	MinLockTime time.Duration
}

type Value struct {
	Host     string `json:"host,omitempty"`
	Pid      int    `json:"pid,omitempty"`
	Random   uint32 `json:"rnd,omitempty"`
	Start    int64  `json:"lock-start,omitempty"`
	TTL      string `json:"lock-ttl,omitempty"`
	Refresh  string `json:"lock-refresh,omitempty"`
	Slots    uint32 `json:"lock-slots,omitempty"`
	Locktime string `json:"lock-time,omitempty"`
}

func (v *Value) String() string {
	b, _ := json.Marshal(v)
	return string(b)
}

type XLock struct {
	m sync.Mutex

	options Options

	etcdClient *etcd.Client

	locked    bool
	lockValue string // uniq identifier of this locker
	lockSlot  int
	lockStart time.Time

	refreshExit chan bool
	refreshWg   sync.WaitGroup
}

var ErrorAlreadyLocked = errors.New("already locked, run Unlock first")
var ErrorNotLocked = errors.New("not locked, run Lock first")
var ErrorLockFailed = errors.New("lock failed")

func DefaultOptions() Options {
	return Options{}
}

// New creates XLock instance
func New(options Options) (*XLock, error) {
	etcdClient, err := etcd.NewClient(options.EtcdEndpoints, options.Debug)

	if err != nil {
		return nil, err
	}

	x := &XLock{
		options:    options,
		etcdClient: etcdClient,
	}

	x.Debug("value: %s", x.lockValue)
	return x, nil
}

func (x *XLock) Debug(format string, v ...interface{}) {
	if x.options.Debug {
		log.Printf(format, v...)
	}
}

func (x *XLock) currentTTL() time.Duration {
	if x.options.MinLockTime == 0 {
		return x.options.TTL
	}

	now := time.Now()
	deadline := now.Add(x.options.TTL)
	minUnlockTime := x.lockStart.Add(x.options.MinLockTime)

	if minUnlockTime.After(deadline) {
		deadline = minUnlockTime
	}

	return deadline.Sub(now)
}

func (x *XLock) lock(ctx context.Context, nowait bool) error {
	x.m.Lock()
	defer x.m.Unlock()

	if x.locked {
		return ErrorAlreadyLocked
	}

	hostname, _ := os.Hostname()

	value := &Value{
		Host:     hostname,
		Pid:      os.Getpid(),
		Random:   uint32(rand.New(rand.NewSource(time.Now().UnixNano())).Int31()),
		TTL:      x.options.TTL.String(),
		Refresh:  x.options.Refresh.String(),
		Slots:    uint32(x.options.Slots),
		Locktime: x.options.MinLockTime.String(),
	}

	x.refreshExit = make(chan bool)
	x.refreshWg = sync.WaitGroup{}

	var etcdIndex uint64

	setIndex := func(index uint64) {
		etcdIndex = index
		x.Debug("etcdIndex := %d", etcdIndex)
	}

	// refresh lock worker function
	startRefresh := func() {
		lockKey := filepath.Join(x.options.Path, fmt.Sprintf("lock-%d", x.lockSlot))

		wg := &x.refreshWg
		exit := x.refreshExit

		wg.Add(1)

		go func() {
			defer wg.Done()

			t := time.NewTicker(x.options.Refresh)

		RefreshLoop:
			for {
				select {
				case <-t.C:
					// refresh key
					x.etcdClient.Query(
						lockKey,
						etcd.PUT(),
						etcd.PrevValue(x.lockValue),
						etcd.PrevExist(true),
						etcd.Refresh(true),
						etcd.TTL(x.currentTTL()),
						etcd.Timeout(x.options.Refresh),
					)
				case <-exit:
					break RefreshLoop
				}
			}
		}()
	}

	// lock function
	acquire := func() (bool, error) {
		x.Debug("acquire %s", x.options.Path)

		for i := 0; i < x.options.Slots; i++ {

			lockKey := filepath.Join(x.options.Path, fmt.Sprintf("lock-%d", i))

			x.lockStart = time.Now()
			value.Start = x.lockStart.Unix()
			x.lockValue = value.String()

			r, err := x.etcdClient.Query(
				lockKey,
				etcd.PUT(),
				etcd.PrevExist(false),
				etcd.Value(x.lockValue),
				etcd.TTL(x.currentTTL()),
				etcd.Context(ctx),
			)

			x.Debug("set %s: (%#v, %#v)", lockKey, r, err)

			if err != nil {
				// context done or server returns bad response
				return false, err
			}

			// update etcdIndex only on first request
			if i == 0 {
				setIndex(r.Index)
			}

			if r.ErrorCode == 0 {
				x.locked = true
				x.lockSlot = i
				x.Debug("SUCCESS locked slot %d", i)
				startRefresh()
				return true, nil
			}
		}

		return false, nil
	}

	// try to lock
	if ok, err := acquire(); ok || (err != nil) {
		return err
	}

	if nowait {
		x.Debug("nowait, lock FAILED")
		return ErrorLockFailed
	}

	for {
		// wait for change otherwise
		x.Debug("wait from index: %d", etcdIndex)
		r, err := x.etcdClient.Query(
			x.options.Path,
			etcd.GET(),
			etcd.Wait(true),
			etcd.WaitIndex(etcdIndex+1),
			etcd.Recursive(true),
			etcd.Timeout(time.Minute),
			etcd.Context(ctx),
		)

		x.Debug("wait response: %#v, err: %#v", r, err)

		// check deadline
		if ctx.Err() != nil {
			x.Debug("ctx.Err(): %s", ctx.Err().Error())
			return ctx.Err()
		}

		if err != nil {
			return err
		}

		if ok, err := acquire(); ok || (err != nil) {
			return err
		}
	}

	return nil
}

func (x *XLock) LockTimeout(t time.Duration) error {
	x.Debug("LockTimeout: %#v", t)
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	return x.lock(ctx, false)
}

func (x *XLock) Lock() error {
	return x.lock(context.Background(), false)
}

func (x *XLock) LockNoWait() error {
	return x.lock(context.Background(), true)
}

func (x *XLock) Unlock() error {
	x.m.Lock()
	defer x.m.Unlock()

	if !x.locked {
		return ErrorNotLocked
	}

	// stop refresher and wait finished
	close(x.refreshExit)
	x.refreshWg.Wait()

	lockKey := filepath.Join(x.options.Path, fmt.Sprintf("lock-%d", x.lockSlot))

	now := time.Now()
	minDeadline := x.lockStart.Add(x.options.MinLockTime)
	if minDeadline.After(now) {
		// don't remove record. just change TTL

		ctx, cancel := context.WithDeadline(context.Background(), minDeadline)
		defer cancel()

		_, err := x.etcdClient.Query(
			lockKey,
			etcd.PUT(),
			etcd.PrevExist(true),
			etcd.PrevValue(x.lockValue),
			etcd.Timeout(time.Second),
			etcd.TTL(minDeadline.Sub(now)),
			etcd.Context(ctx),
			etcd.Refresh(true),
		)

		return err
	}

	// unlock timeout
	ctx, cancel := context.WithTimeout(context.Background(), x.options.TTL)
	defer cancel()

	// do unlock
	_, err := x.etcdClient.Query(
		lockKey,
		etcd.DELETE(),
		etcd.PrevExist(true),
		etcd.PrevValue(x.lockValue),
		etcd.Timeout(time.Second),
		etcd.Context(ctx),
	)

	return err
}