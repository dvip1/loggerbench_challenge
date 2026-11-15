package main

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"math"
)

// Binary reading packet sent to POST /api/v1/logger/data.
// All numbers are little-endian.
//
//	offset  size  field
//	------  ----  -----------------------------------------------
//	0       1     version (= 1)
//	1       8     srno, 16 hex chars decoded to 8 raw bytes
//	9       12    nonce, random for every packet
//	21      280   AES-128-GCM ciphertext (264 bytes) + tag (16 bytes)
//	              additional authenticated data = bytes 0..8 (version + srno)
//
// Plaintext inside the ciphertext (264 bytes):
//
//	offset  size  field
//	0       8     timestamp, Unix seconds (int64)
//	8       256   Sensor1 .. Sensor64, float32 each
//
// Total packet size: 301 bytes (the JSON version was ~1,150 bytes).

// encodeReading builds one encrypted packet.
func encodeReading(aead cipher.AEAD, srno []byte, ts int64, sensors []float32) []byte {
	return encodePacket(aead, ProtocolVersion, srno, ts, sensors)
}

// encodePacket is encodeReading with a chosen version byte (the checks use it to send a bad version).
func encodePacket(aead cipher.AEAD, version byte, srno []byte, ts int64, sensors []float32) []byte {
	// Plaintext: timestamp + sensor values.
	plain := make([]byte, PlaintextSize)
	binary.LittleEndian.PutUint64(plain[0:8], uint64(ts))
	for i, v := range sensors {
		binary.LittleEndian.PutUint32(plain[8+i*4:], math.Float32bits(v))
	}

	// Header: version + srno + nonce (sent in the clear).
	header := make([]byte, HeaderSize)
	header[0] = version
	copy(header[1:9], srno)
	nonce := header[9:21]
	rand.Read(nonce)

	// Packet = header + Seal(plaintext). version+srno are authenticated too,
	// so nobody can change which device a packet claims to come from.
	packet := make([]byte, HeaderSize, PacketSize)
	copy(packet, header)
	return aead.Seal(packet, nonce, plain, header[:9])
}
