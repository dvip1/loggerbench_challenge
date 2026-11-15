package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"
)

var locations = []string{"Mumbai", "Delhi", "Bengaluru", "Pune", "Chennai", "Hyderabad", "Kolkata", "Ahmedabad"}

// RegisterRequest is the body of POST /api/v1/logger/register.
type RegisterRequest struct {
	Srno       string `json:"srno"`
	DeviceName string `json:"device_name"`
	Location   string `json:"location"`
	Token      string `json:"token"`
}

// RegisterResponse is what the server sends back: the device's encryption key.
type RegisterResponse struct {
	Key string `json:"key"` // base64 of 16 random bytes (AES-128)
}

// Device is one simulated physical logger.
type Device struct {
	Srno     string
	Name     string
	Location string
	Token    string
	Owner    *User // the user who created Token; nil when the token came from -tokens

	srnoBytes []byte      // srno as 8 raw bytes, used in packets
	key       []byte      // AES-128 key from the last Register
	aead      cipher.AEAD // AES-128-GCM with key
	sensors   [SensorCount]float64

	lastTs    int64        // timestamp of the last reading sent
	lastAcked atomic.Int64 // timestamp of the last reading the server accepted
	audit     *Audit       // set on the few devices whose readings are read back and compared
}

// NewDevice creates device number i that registers with the given token.
// The token is created by a user in the dashboard and decides which user
// (and which group) the device belongs to.
// The same token + i always gives the same srno, so restarting the mock
// reuses the same devices, and different tokens never share an srno.
func NewDevice(i int, token string) *Device {
	idHash := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", token, i)))
	srnoBytes := idHash[:SrnoSize]

	d := &Device{
		Srno:      hex.EncodeToString(srnoBytes),
		Name:      fmt.Sprintf("logger-%05d", i),
		Location:  locations[i%len(locations)],
		Token:     token,
		srnoBytes: srnoBytes,
	}
	for s := range d.sensors {
		d.sensors[s] = 20 + rand.Float64()*60 // starting value between 20 and 80
	}
	return d
}

// nextSensors moves every sensor a little (random walk) and returns the values.
func (d *Device) nextSensors() []float32 {
	values := make([]float32, SensorCount)
	for s := range d.sensors {
		d.sensors[s] += rand.Float64() - 0.5 // drift by at most ±0.5
		values[s] = float32(math.Round(d.sensors[s]*100) / 100)
	}
	return values
}

// Register sends the registration request and stores the key the server returns.
// Registering again (e.g. after a restart) gives the device a new key.
func (d *Device) Register(client *http.Client, baseURL string) error {
	reqBody, _ := json.Marshal(RegisterRequest{Srno: d.Srno, DeviceName: d.Name, Location: d.Location, Token: d.Token})
	status, respBody, err := post(client, baseURL+RegisterPath, "application/json", reqBody)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("register %s: %w", d.Srno, &StatusError{Code: status, Body: string(respBody)})
	}

	var resp RegisterResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("register %s: bad response: %w", d.Srno, err)
	}
	key, err := base64.StdEncoding.DecodeString(resp.Key)
	if err != nil || len(key) != KeySize {
		return fmt.Errorf("register %s: key must be %d bytes, base64 encoded", d.Srno, KeySize)
	}
	block, _ := aes.NewCipher(key)
	d.key = key
	d.aead, _ = cipher.NewGCM(block)
	return nil
}

// SendData sends one encrypted reading and records the result in stats.
func (d *Device) SendData(client *http.Client, baseURL string, stats *Stats) {
	// Timestamps of one device always go up, even if two sends land in the same second.
	ts := max(time.Now().Unix(), d.lastTs+1)
	d.lastTs = ts
	sensors := d.nextSensors()
	packet := encodeReading(d.aead, d.srnoBytes, ts, sensors)
	if d.audit != nil {
		d.audit.recordSent(ts, sensors)
	}

	start := time.Now()
	status, _, err := post(client, baseURL+DataPath, BinaryContentType, packet)
	ok := err == nil && status >= 200 && status < 300
	if ok {
		d.lastAcked.Store(ts)
		if d.audit != nil {
			d.audit.recordAcked(ts)
		}
	}
	stats.Record(ok, time.Since(start))
}

// Run sends a reading every SendInterval until ctx is cancelled.
func (d *Device) Run(ctx context.Context, client *http.Client, baseURL string, stats *Stats) {
	// Random start offset so thousands of devices don't all fire at the same moment.
	offset := time.Duration(rand.Int64N(int64(SendInterval)))
	select {
	case <-time.After(offset):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(SendInterval)
	defer ticker.Stop()
	for {
		d.SendData(client, baseURL, stats)
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// post sends body and returns the status code and response body.
func post(client *http.Client, url, contentType string, body []byte) (int, []byte, error) {
	resp, err := client.Post(url, contentType, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body) // read fully so the connection can be reused
	return resp.StatusCode, respBody, err
}
