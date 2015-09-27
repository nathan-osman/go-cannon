package queue

import (
	"github.com/nathan-osman/go-cannon/util"

	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/smtp"
	"net/textproto"
	"os"
	"sync"
	"syscall"
	"time"
)

// Persistent connection to an SMTP host.
type Host struct {
	sync.Mutex
	host         string
	storage      *Storage
	lastActivity time.Time
	newMessage   *util.NonBlockingChan
	stop         chan bool
}

// Log the specified message for the specified host.
func (h *Host) log(msg string) {
	log.Printf("[%s] %s", h.host, msg)
}

// Receive the next message in the queue. The host queue is considered
// "inactive" while waiting for new messages to arrive. The current time is
// recorded before entering the select{} block so that the Idle() method can
// calculate the idle time.
func (h *Host) receiveMessage() *Message {
	h.Lock()
	h.lastActivity = time.Now()
	h.Unlock()
	defer func() {
		h.Lock()
		h.lastActivity = time.Time{}
		h.Unlock()
	}()
	for {
		select {
		case i := <-h.newMessage.Recv:
			return i.(*Message)
		case <-h.stop:
			return nil
		}
	}
}

// Attempt to connect to the specified server. The connection attempt is
// performed in a separate goroutine, allowing it to be aborted if the host
// queue is shut down.
func (h *Host) tryMailServer(server string) (*smtp.Client, error) {
	var (
		c    *smtp.Client
		err  error
		done = make(chan bool)
	)
	go func() {
		c, err = smtp.Dial(fmt.Sprintf("%s:25", server))
		close(done)
	}()
	select {
	case <-done:
	case <-h.stop:
		return nil, nil
	}
	if err == nil {
		if hostname, err := os.Hostname(); err == nil {
			if err := c.Hello(hostname); err != nil {
				return nil, err
			}
		}
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: server}); err != nil {
				return nil, err
			}
		}
		return c, nil
	} else {
		return nil, err
	}
}

// Attempt to connect to one of the mail servers.
func (h *Host) connectToMailServer() (*smtp.Client, error) {
	for _, s := range util.FindMailServers(h.host) {
		if c, err := h.tryMailServer(s); err == nil {
			return c, nil
		} else {
			h.log(fmt.Sprintf("unable to connect to %s", s))
		}
	}
	return nil, errors.New("unable to connect to a mail server")
}

// Attempt to send the specified message to the specified client.
func (h *Host) deliverToMailServer(c *smtp.Client, m *Message) error {
	if r, err := h.storage.GetMessageBody(m); err == nil {
		if err := c.Mail(m.From); err != nil {
			return err
		}
		for _, t := range m.To {
			if err := c.Rcpt(t); err != nil {
				return err
			}
		}
		if w, err := c.Data(); err == nil {
			if _, err := io.Copy(w, r); err != nil {
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		} else {
			return err
		}
		return r.Close()
	} else {
		return err
	}
}

// Receive message and deliver them to their recipients. Due to the complicated
// algorithm for message delivery, the body of the method is broken up into a
// sequence of labeled sections.
func (h *Host) run() {
	var (
		m        *Message
		c        *smtp.Client
		err      error
		tries    int
		duration time.Duration
	)
receive:
	if m == nil {
		if m = h.receiveMessage(); m == nil {
			goto shutdown
		}
		h.log("message received in queue")
	}
deliver:
	if c == nil {
		h.log("connecting to mail server...")
		c, err = h.connectToMailServer()
		if c == nil {
			if err != nil {
				h.log(err.Error())
				goto wait
			} else {
				goto shutdown
			}
		}
		h.log("connection established")
	}
	if err := h.deliverToMailServer(c, m); err == nil {
		h.log("mail delivered successfully")
	} else {
		h.log(err.Error())
		if _, ok := err.(syscall.Errno); ok {
			c = nil
			goto deliver
		} else if e, ok := err.(*textproto.Error); ok {
			if e.Code >= 400 && e.Code <= 499 {
				c.Close()
				c = nil
				goto wait
			} else {
				c.Reset()
			}
		}
	}
cleanup:
	h.log("deleting message from disk")
	if err := h.storage.DeleteMessage(m); err != nil {
		h.log(err.Error())
	}
	m = nil
	tries = 0
	goto receive
wait:
	tries++
	// We differ a tiny bit from the RFC spec here but this should work well
	// enough - retry once after a minute, twice on the half-hour, and 16 more
	// times every three hours. This is roughly 48 hours.
	switch {
	case tries == 1:
		duration = time.Minute
	case tries < 4:
		duration = 30 * time.Minute
	case tries < 20:
		duration = 3 * time.Hour
	default:
		h.log("maximum retry count exceeded")
		goto cleanup
	}
	select {
	case <-h.stop:
	case <-time.After(duration):
		goto receive
	}
shutdown:
	h.log("shutting down queue")
	if c != nil {
		c.Close()
	}
	close(h.stop)
}

// Create a new host connection.
func NewHost(host string, storage *Storage) *Host {
	h := &Host{
		host:       host,
		storage:    storage,
		newMessage: util.NewNonBlockingChan(),
		stop:       make(chan bool),
	}
	go h.run()
	return h
}

// Attempt to deliver a message to the host.
func (h *Host) Deliver(m *Message) {
	h.newMessage.Send <- m
}

// Retrieve the connection idle time.
func (h *Host) Idle() time.Duration {
	h.Lock()
	defer h.Unlock()
	if h.lastActivity.IsZero() {
		return 0
	} else {
		return time.Since(h.lastActivity)
	}
}

// Close the connection to the host.
func (h *Host) Stop() {
	h.stop <- true
	<-h.stop
}
