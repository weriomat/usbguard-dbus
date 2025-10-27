package main

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/godbus/dbus/v5"
)

// Dbus generated msg
type dWrite struct {
	id   int
	name string
}

// http api generated msg
type dRead struct {
	resp chan int
}

// Replace with a direct canel and a global counter -> for early reject?
type Manager struct {
	hub        *Hub
	queue      *list.List
	state      chan int
	dbus       chan dWrite
	dbusRemove chan dWrite
	requests   chan dRead
}

func newManager() *Manager {
	return &Manager{
		queue: list.New(),
		// Queue
		state: make(chan int, 100),
		// add item to queue
		dbus:       make(chan dWrite),
		dbusRemove: make(chan dWrite),
		// request item
		requests: make(chan dRead),
		hub:      nil,
	}
}

func (m *Manager) addHub(h *Hub) {
	m.hub = h
}

func (m *Manager) start(ctx context.Context, logger log.Logger) error {
	for {
		select {
		case write := <-m.dbus:
			m.queue.PushBack(write)
		case remove := <-m.dbusRemove:
			// remove the id from the list as it was removed from the device
			var r *list.Element
			for e := m.queue.Front(); e != nil; e = e.Next() {
				if e.Value == remove {
					r = e
				}
			}

			// id could have been accepted already so we must check for nil
			if r != nil {
				m.queue.Remove(r)
			}

		case read := <-m.requests:
			var val dWrite
			f := m.queue.Front()
			if f == nil {
				val = dWrite{
					id:   -1,
					name: "",
				}
			} else {
				m.queue.Remove(f)
				val = f.Value.(dWrite)
			}
			read.resp <- val.id
		case <-ctx.Done():
			level.Info(logger).Log("msg", "finished manager")
			return nil
		}

		// Update WS Clients
		if m.hub != nil {
			var name string
			if m.queue.Len() > 0 {
				name = m.queue.Front().Value.(dWrite).name
			} else {
				name = ""
			}

			notification, _ := json.Marshal(map[string]string{"text": name})
			m.hub.broadcastMessage(notification, nil)
		}
	}
}

func (m *Manager) addEntry(id int, name string) {
	a := dWrite{
		id:   id,
		name: name,
	}

	m.dbus <- a
}

func (m *Manager) removeEntry(id int, name string) {
	a := dWrite{
		id:   id,
		name: name,
	}

	m.dbusRemove <- a
}

func (m *Manager) requestEntry() (int, error) {
	if m.queue.Len() <= 0 {
		return 0, ErrNoItems
	}

	cc := dRead{
		resp: make(chan int),
	}

	m.requests <- cc

	id := <-cc.resp

	return id, nil
}

// arg should be either 0: accept/ 2: reject
func (m *Manager) makeDbusCall(arg int) (string, error) {
	id, err := m.requestEntry()

	if err != nil {
		return "", err
	}

	ids := fmt.Sprintf("{ID: %d}", id)

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// arguments: id of device, 0 == accept, 2 == reject, non permanent: https://github.com/USBGuard/usbguard/blob/main/src/DBus/DBusInterface.xml#L141
	conn.Object("org.usbguard1", "/org/usbguard1/Devices").Call("org.usbguard.Devices1.applyDevicePolicy", 0, uint32(id), uint32(arg), false)
	return ids, nil
}

func (m *Manager) reject() (string, error) {
	id, err := m.makeDbusCall(2)

	if err != nil {
		return "", err
	}

	return id, nil
}

func (m *Manager) accept() (string, error) {
	id, err := m.makeDbusCall(0)

	if err != nil {
		return "", err
	}

	return id, nil
}
