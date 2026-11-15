package main

import "time"

// Device endpoints. BaseURL can be overridden at runtime with the -url flag.
const (
	BaseURL      = "http://localhost:8080"
	RegisterPath = "/api/v1/logger/register"
	DataPath     = "/api/v1/logger/data"
	HealthPath   = "/api/v1/logger/health"
)

// User endpoints. The benchmark uses them to create its own users and tokens
// and to read data back, so it can check that what was accepted was really stored.
const (
	SignupPath  = "/api/v1/auth/signup"
	LoginPath   = "/api/v1/auth/login"
	TokensPath  = "/api/v1/tokens"  // POST = create, DELETE /{id} = revoke
	DevicesPath = "/api/v1/devices" // GET /{srno}/latest, GET /{srno}/readings
)

// Device behaviour.
const (
	SendInterval = 1 * time.Second // each device sends one reading per interval
	SensorCount  = 64              // Sensor1 .. Sensor64

	RequestTimeout      = 5 * time.Second
	RegisterConcurrency = 200 // how many devices register at the same time
)

// Pass rules of a benchmark run.
const (
	MaxIngestP99   = 500 * time.Millisecond // p99 latency of POST /logger/data
	MinSendRate    = 0.99                   // devices must really get ~1 reading/s through
	MaxQueryP99    = 1 * time.Second        // p99 latency of the read endpoints while under load
	FreshnessLimit = 5 * time.Second        // an accepted reading must be readable within this time

	AuditDevices     = 100                    // random devices whose every reading is read back and compared
	ViewerInterval   = 200 * time.Millisecond // how often the simulated dashboard user queries
	HistoryPageLimit = 1000                   // largest `limit` of the readings endpoint

	// Crash test (-crash-test): after the server is back, give it a moment, then expect zero failures.
	RecoveryGrace  = 3 * time.Second
	RecoveryWindow = 10 * time.Second
)

// Binary packet layout. See protocol.go for the full description.
const (
	ProtocolVersion = 1
	KeySize         = 16 // AES-128 key, received from the server on register

	SrnoSize      = 8                                    // srno: 16 hex chars sent as 8 raw bytes
	NonceSize     = 12                                   // AES-GCM nonce
	HeaderSize    = 1 + SrnoSize + NonceSize             // version + srno + nonce = 21
	PlaintextSize = 8 + SensorCount*4                    // timestamp + 64 float32 = 264
	TagSize       = 16                                   // AES-GCM authentication tag
	PacketSize    = HeaderSize + PlaintextSize + TagSize // = 301 bytes

	BinaryContentType = "application/octet-stream"
)
