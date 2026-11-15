package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// checker runs the conformance checks: small, exact tests of the rules in the
// challenge description. They run once before the load starts.
type checker struct {
	client  *http.Client
	baseURL string
	passed  int
	total   int
}

func (c *checker) run(name string, fn func() error) bool {
	c.total++
	if err := fn(); err != nil {
		fmt.Printf("  FAIL  %s\n        %v\n", name, err)
		return false
	}
	c.passed++
	fmt.Printf("  ok    %s\n", name)
	return true
}

// wantRejected turns "the server said 4xx" into success and everything else into an error.
func wantRejected(what string, err error) error {
	var se *StatusError
	if errors.As(err, &se) && se.Code >= 400 && se.Code < 500 {
		return nil
	}
	if err == nil {
		return fmt.Errorf("%s: expected a 4xx status, got success", what)
	}
	return fmt.Errorf("%s: expected a 4xx status, got: %v", what, err)
}

// eventually retries fn until it succeeds or FreshnessLimit has passed.
func eventually(fn func() error) error {
	deadline := time.Now().Add(FreshnessLimit)
	for {
		err := fn()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// sendPacket posts a raw packet to the data endpoint.
func (c *checker) sendPacket(packet []byte) error {
	status, body, err := post(c.client, c.baseURL+DataPath, BinaryContentType, packet)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &StatusError{Code: status, Body: string(bytes.TrimSpace(body))}
	}
	return nil
}

// history fetches a device's readings and checks they are exactly the expected ones, in order.
func (c *checker) history(u *User, d *Device, from, to int64, limit int, want []sentReading) error {
	rows, err := u.Readings(c.client, c.baseURL, d.Srno, from, to, limit)
	if err != nil {
		return err
	}
	if len(rows) != len(want) {
		return fmt.Errorf("readings from=%d to=%d limit=%d: got %d readings, want %d", from, to, limit, len(rows), len(want))
	}
	for i, row := range rows {
		if !row.matches(want[i].ts, want[i].sensors) {
			return fmt.Errorf("reading %d (timestamp %d) is not what the device sent at %d", i, row.Timestamp, want[i].ts)
		}
	}
	return nil
}

type sentReading struct {
	ts      int64
	sensors []float32
}

// runChecks returns how many checks passed and how many there are.
func runChecks(client *http.Client, baseURL string) (passed, total int) {
	c := &checker{client: client, baseURL: baseURL}
	done := func() (int, int) { return c.passed, c.total }
	fmt.Println("conformance checks:")

	// ---- users and tokens ----
	var alice, bob *User
	var tokA, tokB *RegToken
	if !c.run("sign up and log in two users", func() (err error) {
		if alice, err = newUser(client, baseURL); err != nil {
			return err
		}
		bob, err = newUser(client, baseURL)
		return err
	}) {
		return done()
	}
	c.run("user endpoints reject a missing or invalid access token", func() error {
		for _, bearer := range []string{"", "not-a-real-token"} {
			err := callJSON(client, "POST", baseURL+TokensPath, bearer, map[string]string{"name": "x"}, nil)
			if err := wantRejected("create token with bearer \""+bearer+"\"", err); err != nil {
				return err
			}
		}
		return nil
	})
	c.run("login rejects a wrong password", func() error {
		probe := &User{Email: alice.Email}
		return wantRejected("login", probe.Login(client, baseURL, "wrong-"+alice.Password))
	})
	if !c.run("create registration tokens", func() (err error) {
		if tokA, err = alice.CreateToken(client, baseURL, "check-a"); err != nil {
			return err
		}
		if tokB, err = bob.CreateToken(client, baseURL, "check-b"); err != nil {
			return err
		}
		if tokA.Token == tokB.Token {
			return fmt.Errorf("two users got the same token")
		}
		return nil
	}) {
		return done()
	}

	// ---- registration ----
	d := NewDevice(0, "check-"+randomHex(8)) // a device nobody has seen before
	c.run("register rejects an unknown token", func() error {
		return wantRejected("register", d.Register(client, baseURL))
	})
	d.Token = tokA.Token
	if !c.run("register returns a 16-byte key", func() error { return d.Register(client, baseURL) }) {
		return done()
	}

	// Readings of the check device: one per second, starting a little in the past.
	t0 := time.Now().Unix() - 20
	var sent []sentReading // accepted readings, oldest first
	next := func() sentReading {
		return sentReading{ts: t0 + int64(len(sent)), sensors: d.nextSensors()}
	}
	send := func(r sentReading) error {
		if err := c.sendPacket(encodeReading(d.aead, d.srnoBytes, r.ts, r.sensors)); err != nil {
			return err
		}
		sent = append(sent, r)
		return nil
	}

	// ---- data and reading it back ----
	first := next()
	firstPacket := encodeReading(d.aead, d.srnoBytes, first.ts, first.sensors)
	if !c.run("data: a valid reading is accepted", func() error {
		if err := c.sendPacket(firstPacket); err != nil {
			return err
		}
		sent = append(sent, first)
		return nil
	}) {
		return done()
	}
	c.run("latest reading comes back exactly as sent", func() error {
		return eventually(func() error {
			r, err := alice.Latest(client, baseURL, d.Srno)
			if err != nil {
				return err
			}
			if !r.matches(first.ts, first.sensors) {
				return fmt.Errorf("latest reading (timestamp %d) differs from the one sent at %d", r.Timestamp, first.ts)
			}
			return nil
		})
	})
	c.run("data: a replayed packet does not create a second reading", func() error {
		c.sendPacket(firstPacket) // 2xx or 4xx are both fine, a duplicate row is not
		for range 3 {
			if err := send(next()); err != nil {
				return err
			}
		}
		return eventually(func() error { return c.history(alice, d, t0, t0+100, HistoryPageLimit, sent) })
	})
	c.run("history: from and to are inclusive, limit returns the oldest readings", func() error {
		if err := c.history(alice, d, sent[1].ts, sent[2].ts, HistoryPageLimit, sent[1:3]); err != nil {
			return err
		}
		return c.history(alice, d, t0, t0+100, 2, sent[:2])
	})

	// ---- bad packets ----
	c.run("data: rejects a tampered packet", func() error {
		r := next()
		packet := encodeReading(d.aead, d.srnoBytes, r.ts, r.sensors)
		packet[HeaderSize+10] ^= 0x01
		return wantRejected("tampered packet", c.sendPacket(packet))
	})
	c.run("data: rejects a wrong packet size", func() error {
		r := next()
		packet := encodeReading(d.aead, d.srnoBytes, r.ts, r.sensors)
		if err := wantRejected("300-byte packet", c.sendPacket(packet[:PacketSize-1])); err != nil {
			return err
		}
		return wantRejected("302-byte packet", c.sendPacket(append(packet, 0)))
	})
	c.run("data: rejects a version other than 1", func() error {
		r := next()
		return wantRejected("version 2 packet", c.sendPacket(encodePacket(d.aead, 2, d.srnoBytes, r.ts, r.sensors)))
	})
	c.run("data: rejects an unregistered device", func() error {
		r := next()
		stranger := NewDevice(1, "check-"+randomHex(8))
		return wantRejected("unknown srno", c.sendPacket(encodeReading(d.aead, stranger.srnoBytes, r.ts, r.sensors)))
	})
	c.run("rejected packets are not stored", func() error {
		return c.history(alice, d, t0, t0+100, HistoryPageLimit, sent)
	})

	// ---- ownership ----
	c.run("a user cannot read another user's device", func() error {
		_, err := bob.Latest(client, baseURL, d.Srno)
		if err := wantRejected("latest as another user", err); err != nil {
			return err
		}
		_, err = bob.Readings(client, baseURL, d.Srno, t0, t0+100, HistoryPageLimit)
		return wantRejected("readings as another user", err)
	})
	c.run("registering again gives a new key and retires the old one", func() error {
		oldKey, oldAEAD := d.key, d.aead
		if err := d.Register(client, baseURL); err != nil {
			return err
		}
		if bytes.Equal(oldKey, d.key) {
			return fmt.Errorf("the second registration returned the same key")
		}
		r := next()
		if err := wantRejected("packet with the old key", c.sendPacket(encodeReading(oldAEAD, d.srnoBytes, r.ts, r.sensors))); err != nil {
			return err
		}
		return send(r)
	})
	c.run("registering with another user's token moves the device and its history", func() error {
		d.Token = tokB.Token
		if err := d.Register(client, baseURL); err != nil {
			return err
		}
		if err := send(next()); err != nil {
			return err
		}
		if err := eventually(func() error { return c.history(bob, d, t0, t0+100, HistoryPageLimit, sent) }); err != nil {
			return fmt.Errorf("new owner: %w", err)
		}
		_, err := alice.Latest(client, baseURL, d.Srno)
		return wantRejected("latest as the previous owner", err)
	})

	// ---- revoking ----
	c.run("a user cannot revoke another user's token", func() error {
		if err := wantRejected("revoke", alice.RevokeToken(client, baseURL, tokB.ID)); err != nil {
			return err
		}
		return send(next()) // the token must still work
	})
	c.run("revoked token: registration and data are rejected, history stays readable", func() error {
		if err := bob.RevokeToken(client, baseURL, tokB.ID); err != nil {
			return err
		}
		err := eventually(func() error {
			r := next()
			if err := c.sendPacket(encodeReading(d.aead, d.srnoBytes, r.ts, r.sensors)); err == nil {
				sent = append(sent, r) // accepted, so it is part of the history
				return fmt.Errorf("data from a device of the revoked token was still accepted")
			} else if err := wantRejected("data", err); err != nil {
				return err
			}
			return wantRejected("register", d.Register(client, baseURL))
		})
		if err != nil {
			return err
		}
		return eventually(func() error { return c.history(bob, d, t0, t0+100, HistoryPageLimit, sent) })
	})
	c.run("a device of a revoked token can come back with a valid token", func() error {
		d.Token = tokA.Token
		if err := d.Register(client, baseURL); err != nil {
			return err
		}
		if err := send(next()); err != nil {
			return err
		}
		return eventually(func() error { return c.history(alice, d, t0, t0+100, HistoryPageLimit, sent) })
	})

	return done()
}
