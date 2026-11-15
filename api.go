package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// User is a dashboard account the benchmark created for itself.
type User struct {
	Email       string
	Password    string
	AccessToken string // from login, sent as "Authorization: Bearer ..."
}

// RegToken is a registration token created by a user.
type RegToken struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	Owner *User  `json:"-"` // nil for tokens passed with -tokens
}

// Reading is one reading as returned by the read endpoints.
type Reading struct {
	Timestamp int64     `json:"timestamp"`
	Sensors   []float64 `json:"sensors"`
}

// matches reports whether the reading is exactly what a device sent.
func (r Reading) matches(ts int64, sensors []float32) bool {
	if r.Timestamp != ts || len(r.Sensors) != len(sensors) {
		return false
	}
	for i, v := range r.Sensors {
		if float32(v) != sensors[i] {
			return false
		}
	}
	return true
}

// StatusError is returned when the server answers with a non-2xx status.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("status %d: %s", e.Code, e.Body) }

// callJSON sends a JSON request and decodes a JSON response into out (if not nil).
// A non-2xx answer is returned as *StatusError.
func callJSON(client *http.Client, method, url, bearer string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, _ := json.Marshal(in)
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Code: resp.StatusCode, Body: string(bytes.TrimSpace(respBody))}
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("bad JSON in response: %w", err)
		}
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// newUser signs up a fresh user and logs in.
func newUser(client *http.Client, baseURL string) (*User, error) {
	u := &User{Email: "bench-" + randomHex(6) + "@example.com", Password: "Bench-" + randomHex(10) + "-1a!"}
	creds := map[string]string{"email": u.Email, "password": u.Password}
	if err := callJSON(client, "POST", baseURL+SignupPath, "", creds, nil); err != nil {
		return nil, fmt.Errorf("signup: %w", err)
	}
	if err := u.Login(client, baseURL, u.Password); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	return u, nil
}

func (u *User) Login(client *http.Client, baseURL, password string) error {
	var resp struct {
		AccessToken string `json:"access_token"`
	}
	creds := map[string]string{"email": u.Email, "password": password}
	if err := callJSON(client, "POST", baseURL+LoginPath, "", creds, &resp); err != nil {
		return err
	}
	if resp.AccessToken == "" {
		return fmt.Errorf("login response has no access_token")
	}
	u.AccessToken = resp.AccessToken
	return nil
}

func (u *User) CreateToken(client *http.Client, baseURL, name string) (*RegToken, error) {
	t := &RegToken{Owner: u}
	if err := callJSON(client, "POST", baseURL+TokensPath, u.AccessToken, map[string]string{"name": name}, t); err != nil {
		return nil, fmt.Errorf("create token: %w", err)
	}
	if t.ID == "" || t.Token == "" {
		return nil, fmt.Errorf("create token: response needs non-empty string fields \"id\" and \"token\"")
	}
	return t, nil
}

func (u *User) RevokeToken(client *http.Client, baseURL, id string) error {
	return callJSON(client, "DELETE", baseURL+TokensPath+"/"+id, u.AccessToken, nil, nil)
}

// Latest returns the newest reading of a device.
func (u *User) Latest(client *http.Client, baseURL, srno string) (Reading, error) {
	var r Reading
	err := callJSON(client, "GET", baseURL+DevicesPath+"/"+srno+"/latest", u.AccessToken, nil, &r)
	return r, err
}

// Readings returns up to limit readings with from <= timestamp <= to, oldest first.
func (u *User) Readings(client *http.Client, baseURL, srno string, from, to int64, limit int) ([]Reading, error) {
	var resp struct {
		Readings []Reading `json:"readings"`
	}
	url := fmt.Sprintf("%s%s/%s/readings?from=%d&to=%d&limit=%d", baseURL, DevicesPath, srno, from, to, limit)
	err := callJSON(client, "GET", url, u.AccessToken, nil, &resp)
	return resp.Readings, err
}

// AllReadings pages through Readings until the whole range is fetched.
func (u *User) AllReadings(client *http.Client, baseURL, srno string, from, to int64) ([]Reading, error) {
	var all []Reading
	for from <= to {
		page, err := u.Readings(client, baseURL, srno, from, to, HistoryPageLimit)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < HistoryPageLimit {
			break
		}
		from = max(from, page[len(page)-1].Timestamp) + 1
	}
	return all, nil
}
