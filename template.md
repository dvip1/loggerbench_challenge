# Challenge: Build a Logger Platform

Thousands of physical data loggers report sensor readings to your server every second.
Their owners log in to a dashboard to watch them. Build the backend and a small frontend
on one tiny server, and make it carry more loggers than anyone else's.

Language, framework, storage, architecture and UI are all your call. A database is optional.
The only fixed things are the HTTP contract below and the box it runs on.

---

## How It Works

1. A user signs up and logs in to your dashboard.
2. The user creates a **registration token**. Each token is a group: all devices registered
   with it belong to that user and that group. A user can create as many tokens as they like.
3. The token is put on the user's devices. Each logger:
   1. checks the server's health,
   2. registers itself with the token and receives an encryption key,
   3. sends one encrypted reading with 64 sensor values **every second**, forever.
4. The user sees their devices and readings in the dashboard.

This repository is the mock device and the judge in one program:

```bash
# Full benchmark: conformance checks, 5 minutes of load from 5000 devices, read-back audit.
# It signs up its own users and creates its own tokens through your API.
go run . -url http://your-server:8080 -devices 5000 -duration 5m

# Only the conformance checks (fast, use it while developing)
go run . -url http://your-server:8080 -devices 0

# Only load, on tokens you created in your own dashboard, to watch your UI with live data
go run . -url http://your-server:8080 -devices 1000 -tokens <token1>,<token2>
```

It ends with one `PASS`/`FAIL` line per part and a final `VERDICT`.

---

## The Box

| | |
|---|---|
| Server | AWS Lightsail, **2 vCPU / 2 GB RAM / 60 GB SSD** (the $12 Linux plan) |
| OS | Ubuntu 24.04, fresh |
| Network | plain HTTP on port `8080`, no TLS |
| Deadline | _TODO(organizer)_ |

Everything runs on this one box: backend, frontend, storage. No managed database, object
storage, queue or any other outside service.

The box is yours to tune. Kernel parameters, swap, filesystem choice, CPU pinning and
anything else are fair, as long as your setup script reproduces it from a fresh OS.

Two things about this instance type are worth knowing before you design:

- **Disk fills fast.** One reading is 264 bytes of plaintext. Stored naively, 1,000 devices
  write about 23 GB a day.
- **The CPU is burstable.** The baseline is 20% per vCPU; above that the instance spends
  burst capacity, and when it runs out you are throttled to the baseline. A full bucket
  lasts for hours, so a benchmark run is fine, but an afternoon of load testing can drain
  it. Check the "CPU burst capacity" graph of the instance before you trust a number.

---

## Device Endpoints (fixed)

### `GET /api/v1/logger/health`
Return `200` with a JSON body when healthy. What you put in it is up to you.

### `POST /api/v1/logger/register`
```json
{
  "srno": "a3f9c2e17b4d8e05",
  "device_name": "logger-00042",
  "location": "Pune",
  "token": "<registration token created by a user>"
}
```
- `srno` is always 16 hex characters.
- Unknown or revoked token: `4xx`.
- The device belongs to the token's owner, in the token's group.
- On success return `2xx` with a new random **AES-128 key** for that device:
  ```json
  {"key": "<base64 of 16 random bytes>"}
  ```
- A device that registers again (e.g. after a reboot) gets a new key. The old key stops working.
- A device that registers with a token of a different user moves to that user, together
  with its history. The previous owner can no longer see it.

### `POST /api/v1/logger/data`
`Content-Type: application/octet-stream`, a fixed **301-byte** packet. All numbers are little-endian.

| Offset | Size | Field |
|---|---|---|
| 0 | 1 | version (`1`) |
| 1 | 8 | srno (the 16 hex chars as 8 raw bytes) |
| 9 | 12 | nonce |
| 21 | 280 | AES-128-GCM ciphertext (264) + tag (16). Additional data = bytes `0..8` |

Decrypted plaintext (264 bytes):

| Offset | Size | Field |
|---|---|---|
| 0 | 8 | timestamp, Unix seconds (int64) |
| 8 | 256 | Sensor1 … Sensor64, `float32` each |

- Return `2xx` when the reading is accepted.
- Return `4xx` for anything else: wrong size, version other than `1`, unknown device,
  a packet that does not decrypt with the device's current key, a device whose token is revoked.
- Timestamps of one device only go up. A packet whose timestamp is not newer than the
  device's newest reading (a replay) may be rejected or ignored, but must never show up
  as a second reading.
- **`2xx` is a promise.** An accepted reading survives `kill -9` of every process of your
  server, and is readable through the API within **5 seconds**.

The mock uses up to 1,000 keep-alive HTTP/1.1 connections shared by all devices.

---

## User Endpoints (fixed)

The judge needs these to act as a user and read data back. Your dashboard may use them
too, or its own endpoints, cookies, websockets, whatever you like.

All endpoints except signup and login need `Authorization: Bearer <access_token>` and
answer `4xx` without a valid one. An access token stays valid for at least 24 hours,
including across server restarts.

| Endpoint | Request | Success response |
|---|---|---|
| `POST /api/v1/auth/signup` | `{"email": "...", "password": "..."}` | `2xx` |
| `POST /api/v1/auth/login` | `{"email": "...", "password": "..."}` | `{"access_token": "..."}` |
| `POST /api/v1/tokens` | `{"name": "..."}` | `{"id": "...", "token": "..."}` (both strings) |
| `DELETE /api/v1/tokens/{id}` | | `2xx`, the token is revoked |
| `GET /api/v1/devices/{srno}/latest` | | `{"timestamp": 1794728950, "sensors": [64 numbers]}` |
| `GET /api/v1/devices/{srno}/readings?from=&to=&limit=` | | `{"readings": [{"timestamp": ..., "sensors": [...]}, ...]}` |

- `readings` returns the readings with `from <= timestamp <= to` (Unix seconds), oldest
  first, at most `limit` of them. `limit` is at most 1000.
- Sensor values come back as exactly the `float32` values the device sent.
- A user only reaches their own tokens and devices. Anyone else's: `4xx`.
- Revoking a token takes effect within 5 seconds: its devices can no longer register or
  send data. Their history stays readable, and a device can come back by registering with
  another valid token.

---

## Dashboard (you design it)

Users must be able to:
- sign up and log in,
- create, name and revoke registration tokens,
- see their devices, grouped by token,
- see the latest reading of a device,
- see a device's history over a time range.

Beyond that, surprise us. Live updates, charts of 64 sensors that stay readable, a fleet
overview that works with 20,000 devices: it is all open.

---

## Rules

1. Every accepted reading is stored and comes back exactly. No sampling, no rounding, no expiry.
2. All persistent state lives in one directory, `./data`, so it can be measured and wiped.
3. The device and user endpoints match this document exactly. The mock will not change.
4. No special treatment of the benchmark: no guessing which devices are audited, no
   deriving serial numbers or sensor values from the mock's source, no behaviour that
   depends on the email address or token name.
5. Starting from a fresh OS, one command from your README installs and starts everything,
   and one command starts it again after a crash.

Anything not forbidden here is allowed. If you find a loophole, use it and write about it.

---

## The Benchmark

A scored run is:

```bash
go run . -url http://<server>:8080 -devices <N> -duration 5m -crash-test
```

| Part | Passes when |
|---|---|
| conformance | every check of the endpoint rules above passes |
| registration | all N devices register |
| ingest | 0 failed requests, every device really gets ~1 reading/s through, p99 latency ≤ 500 ms |
| queries under load | a simulated user opens random devices 5 times a second during the load: 0 errors, nothing older than 5 s, p99 ≤ 1 s |
| audit | 100 random devices have every accepted reading read back and compared: none missing, changed or duplicated |
| crash recovery | you `kill -9` the server under load and restart it; devices carry on with the keys they had, and the audit still passes |

`VERDICT: PASS at N devices` is the only result that counts.

---

## Scoring

To be ranked you need a passing scored run at **1,000 devices or more**.

| Category | Weight | How it is measured |
|---|---|---|
| Scale | 40 | the highest N with a passing scored run (three attempts on scoring day, you pick the N) |
| Storage | 20 | size of `./data` one minute after that run, divided by `accepted readings` from the output |
| Query speed | 10 | `queries under load` p99 of that run |
| Dashboard | 20 | everyone ranks everyone else's dashboard |
| Idea and write-up | 10 | everyone ranks everyone else's README: the most interesting design, best explained |

In each category the best entry gets the full weight, the last gets 0, the rest are spread
evenly between. Nobody ranks their own entry.

### Scoring day

The organizer competes too, so the tool is the judge and the runs happen in the open:

- One Lightsail instance is used for everyone, wiped between entries, with its CPU burst
  capacity above 50% before each run.
- The mock runs on a second, larger machine in the same region (8 vCPU or more, so the
  generator is never the bottleneck).
- Runs happen on a call, or are recorded, so anyone can see the output.

---

## Submission

- Repository link
- README with:
  - the one command that installs and starts it, and the one that restarts it,
  - your architecture and why you chose it,
  - screenshots of the dashboard,
  - your own benchmark results (N, latencies, bytes per reading, CPU/RAM),
  - what you tried that **didn't** work.
