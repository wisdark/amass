package eventbus

import (
	"reflect"
	"sync"
	"time"

	"github.com/OWASP/Amass/v3/queue"
	"github.com/OWASP/Amass/v3/semaphore"
)

// The priority levels for event bus messages.
const (
	PriorityLow int = iota
	PriorityHigh
	PriorityCritical
)

type pubReq struct {
	Topic string
	Args  []reflect.Value
}

type subReq struct {
	Topic string
	Fn    interface{}
}

type eventbusChans struct {
	Subscribe   chan *subReq
	Unsubscribe chan *subReq
}

// EventBus handles sending and receiving events across Amass.
type EventBus struct {
	channels *eventbusChans
	max      semaphore.Semaphore
	queues   []*queue.Queue
	done     chan struct{}
	closed   sync.Once
}

// NewEventBus initializes and returns an EventBus object.
func NewEventBus(max int) *EventBus {
	eb := &EventBus{
		channels: &eventbusChans{
			Subscribe:   make(chan *subReq, 10),
			Unsubscribe: make(chan *subReq, 10),
		},
		max: semaphore.NewSimpleSemaphore(max),
		queues: []*queue.Queue{
			new(queue.Queue),
			new(queue.Queue),
			new(queue.Queue),
		},
		done: make(chan struct{}, 2),
	}

	go eb.processRequests(eb.channels)
	return eb
}

// Stop prevents any additional requests from being sent.
func (eb *EventBus) Stop() {
	eb.closed.Do(func() {
		close(eb.done)
	})
}

// Subscribe registers callback to be executed for all requests on the channel.
func (eb *EventBus) Subscribe(topic string, fn interface{}) {
	eb.channels.Subscribe <- &subReq{
		Topic: topic,
		Fn:    fn,
	}
}

// Unsubscribe deregisters the callback from the channel.
func (eb *EventBus) Unsubscribe(topic string, fn interface{}) {
	eb.channels.Unsubscribe <- &subReq{
		Topic: topic,
		Fn:    fn,
	}
}

// Publish sends req on the channel labeled with name.
func (eb *EventBus) Publish(topic string, priority int, args ...interface{}) {
	if topic != "" && priority >= PriorityLow && priority <= PriorityCritical {
		passedArgs := make([]reflect.Value, 0)

		for _, arg := range args {
			passedArgs = append(passedArgs, reflect.ValueOf(arg))
		}

		eb.queues[priority].Append(&pubReq{
			Topic: topic,
			Args:  passedArgs,
		})
	}
}

func (eb *EventBus) processRequests(chs *eventbusChans) {
	topics := make(map[string][]reflect.Value)
	curIdx := 0
	maxIdx := 6
	delays := []int{10, 25, 50, 75, 100, 150, 250}
loop:
	for {
		select {
		case <-eb.done:
			return
		case sub := <-chs.Subscribe:
			if sub.Topic != "" && reflect.TypeOf(sub.Fn).Kind() == reflect.Func {
				callback := reflect.ValueOf(sub.Fn)

				topics[sub.Topic] = append(topics[sub.Topic], callback)
			}
		case unsub := <-chs.Unsubscribe:
			if unsub.Topic != "" && reflect.TypeOf(unsub.Fn).Kind() == reflect.Func {
				callback := reflect.ValueOf(unsub.Fn)

				var channels []reflect.Value
				for _, c := range topics[unsub.Topic] {
					if c != callback {
						channels = append(channels, c)
					}
				}

				topics[unsub.Topic] = channels
			}
		default:
			var found bool
			var element interface{}
			// Pull from the critical queue first
			for p := PriorityCritical; p >= PriorityLow; p-- {
				element, found = eb.queues[p].Next()
				if found {
					break
				}
			}

			if !found {
				if curIdx < maxIdx {
					curIdx++
				}
				time.Sleep(time.Duration(delays[curIdx]) * time.Millisecond)
				continue loop
			}

			curIdx = 0
			p := element.(*pubReq)
			callbacks, found := topics[p.Topic]
			if !found {
				continue loop
			}

			for _, cb := range callbacks {
				eb.max.Acquire(1)
				go eb.execute(cb, p.Args)
			}
		}
	}
}

func (eb *EventBus) execute(callback reflect.Value, args []reflect.Value) {
	defer eb.max.Release(1)

	callback.Call(args)
}
