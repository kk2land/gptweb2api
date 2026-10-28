package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Implements the documented MCP Events webhook subset. It never opens an
// inbound HTTP endpoint. Enabling discovery does not make the dots model ready:
// a signed calibration event must be claimed and completed first.
type Events struct {
	m      *Manager
	owner  string
	client *http.Client
}
type Subscription struct {
	ID, Owner, URL, Secret string
	Expires                time.Time
}
type EventParams struct {
	mcp.ParamsBase
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Delivery  struct {
		Mode   string `json:"mode"`
		URL    string `json:"url"`
		Secret string `json:"secret"`
	} `json:"delivery"`
	ID     string  `json:"id"`
	Cursor *string `json:"cursor"`
}
type EventResult struct {
	mcp.ResultBase
	Value map[string]any
}

func (r *EventResult) MarshalJSON() ([]byte, error) { return json.Marshal(r.Value) }
func (r *EventResult) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &r.Value) }
func eventRPCError(reason string) error {
	b, _ := json.Marshal(map[string]string{"reason": reason})
	return &jsonrpc.Error{Code: -32015, Message: "Callback endpoint verification failed", Data: b}
}

func NewEvents(m *Manager, tunnelID string) *Events {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second, DialContext: publicDial}
	return &Events{m: m, owner: tunnelID, client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var deniedNetworks = []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "2001:db8::/32", "2001::/32", "2002::/16"}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, raw := range deniedNetworks {
		if netip.MustParsePrefix(raw).Contains(ip) {
			return false
		}
	}
	return true
}
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("callback DNS unavailable")
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, errors.New("callback destination is not public")
		}
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}
func validateCallback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return eventRPCError("invalid_url")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicIP(ip) {
		return eventRPCError("invalid_address")
	}
	return nil
}
func signingKey(secret string) ([]byte, error) {
	if !strings.HasPrefix(secret, "whsec_") {
		return nil, errors.New("invalid signing secret")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) < 24 || len(key) > 64 {
		return nil, errors.New("invalid signing secret")
	}
	return key, nil
}
func webhookSignature(secret, id, timestamp string, body []byte) (string, error) {
	key, err := signingKey(secret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%s.%s.", id, timestamp)
	_, _ = mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}
func (e *Events) send(ctx context.Context, s Subscription, id string, body []byte) ([]byte, error) {
	if len(body) > 256*1024 {
		return nil, errors.New("event exceeds 256 KiB")
	}
	if err := validateCallback(s.URL); err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	signature, err := webhookSignature(s.Secret, id, ts, body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", signature)
	req.Header.Set("X-MCP-Subscription-Id", s.ID)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, eventRPCError("timeout_or_connection")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024+1))
	if err != nil || len(b) > 256*1024 {
		return nil, eventRPCError("invalid_response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, eventRPCError("http_status")
	}
	return b, nil
}
func (e *Events) subscribe(ctx context.Context, p *EventParams) (*EventResult, error) {
	if p.Name != "task.available" || len(p.Arguments) != 1 || p.Arguments["queue"] != "default" || p.Delivery.Mode != "webhook" || p.Cursor != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "subscribe to task.available with queue=default and webhook delivery; replay is unsupported"}
	}
	if _, err := signingKey(p.Delivery.Secret); err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "secret must be whsec_ followed by base64 encoding of 24 to 64 bytes"}
	}
	if err := validateCallback(p.Delivery.URL); err != nil {
		return nil, err
	}
	s := Subscription{ID: "sub_" + digest(e.owner+"\x00"+p.Delivery.URL+"\x00task.available\x00{\"queue\":\"default\"}"), Owner: e.owner, URL: p.Delivery.URL, Secret: p.Delivery.Secret, Expires: time.Now().Add(24 * time.Hour)}
	challenge := randomID("challenge_")
	body, _ := json.Marshal(map[string]string{"type": "verification", "challenge": challenge})
	reply, err := e.send(ctx, s, randomID("msg_verification_"), body)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(reply, &answer) != nil || subtle.ConstantTimeCompare([]byte(challenge), []byte(answer.Challenge)) != 1 {
		return nil, eventRPCError("challenge_failed")
	}
	b, _ := json.Marshal(s)
	e.m.mu.Lock()
	_, err = e.m.store.db.Exec(`INSERT INTO subscriptions VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET expires=excluded.expires,payload=excluded.payload`, s.ID, s.Expires.UnixMilli(), b)
	e.m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err = e.m.ensureDotCalibration(); err != nil {
		return nil, err
	}
	return &EventResult{Value: map[string]any{"id": s.ID, "refreshBefore": s.Expires.UTC().Format(time.RFC3339), "cursor": nil, "truncated": false}}, nil
}
func (e *Events) subscription(id string) (Subscription, error) {
	var s Subscription
	var b []byte
	if err := e.m.store.db.QueryRow(`SELECT payload FROM subscriptions WHERE id=? AND expires>?`, id, time.Now().UnixMilli()).Scan(&b); err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if s.Owner != e.owner {
		return s, errors.New("subscription owner mismatch")
	}
	return s, nil
}
func (e *Events) Register(s *mcp.Server) error {
	if !e.m.cfg.Dots.Enabled {
		return nil
	}
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			if err != nil || method != "server/discover" {
				return result, err
			}
			b, err := json.Marshal(result)
			if err != nil {
				return nil, err
			}
			var value map[string]any
			if err = json.Unmarshal(b, &value); err != nil {
				return nil, err
			}
			caps, ok := value["capabilities"].(map[string]any)
			if !ok {
				return nil, errors.New("invalid discovery capabilities")
			}
			caps["events"] = map[string]any{}
			return &EventResult{Value: value}, nil
		}
	})
	if err := mcp.AddReceivingCustomMethod(s, "events/list", func(ctx context.Context, ss *mcp.ServerSession, p *EventParams) (*EventResult, error) {
		return &EventResult{Value: map[string]any{"events": []any{map[string]any{"name": "task.available", "description": "An independent API task is available. Claim it with claim_task; renew the 90 second lease while working and complete it with complete_task. The task contains its complete messages and bound workdir.", "delivery": []string{"webhook"}, "inputSchema": objectSchema(map[string]any{"queue": map[string]any{"type": "string", "enum": []string{"default"}}}, []string{"queue"}), "payloadSchema": objectSchema(map[string]any{"request_id": stringSchema(), "queue": stringSchema()}, []string{"request_id", "queue"})}}}}, nil
	}); err != nil {
		return err
	}
	if err := mcp.AddReceivingCustomMethod(s, "events/subscribe", func(ctx context.Context, ss *mcp.ServerSession, p *EventParams) (*EventResult, error) {
		return e.subscribe(ctx, p)
	}); err != nil {
		return err
	}
	if err := mcp.AddReceivingCustomMethod(s, "events/unsubscribe", func(ctx context.Context, ss *mcp.ServerSession, p *EventParams) (*EventResult, error) {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		_, err := e.m.store.db.Exec(`DELETE FROM subscriptions WHERE id=?`, p.ID)
		e.m.models[ModelDots] = false
		e.m.signalLocked()
		return &EventResult{Value: map[string]any{}}, err
	}); err != nil {
		return err
	}
	addTool(s, "claim_task", "Atomically claim an independent dots task using an active subscription. Returns complete messages, bound workdir, short-lived turn_token, and lease expiry. Never execute another backend's tasks.", objectSchema(map[string]any{"request_id": stringSchema(), "subscription_id": stringSchema()}, []string{"request_id", "subscription_id"}), func(ctx context.Context, a map[string]json.RawMessage) (any, error) {
		if _, err := e.subscription(str(a, "subscription_id")); err != nil {
			return nil, fault("subscription_expired", "active verified subscription required", 409)
		}
		return e.m.claimDot(ctx, str(a, "request_id"), str(a, "subscription_id"))
	})
	addTool(s, "renew_task", "Renew the current dots task lease for at most 90 seconds, bounded by the task deadline.", credentialSchema(nil, nil), func(ctx context.Context, a map[string]json.RawMessage) (any, error) {
		return e.m.renewDot(credentials(a))
	})
	return nil
}

func (e *Events) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := e.deliver(ctx); err != nil {
				e.m.log.Warn("Dot event delivery pending", "error_code", safeErrorCode(err))
			}
		}
	}
}
func (e *Events) deliver(ctx context.Context) error {
	// Persist each event's exact bytes and stable ID before the first attempt.
	e.m.mu.Lock()
	if e.m.dotBusy {
		e.m.mu.Unlock()
		return nil
	}
	rows, err := e.m.store.db.Query(`SELECT payload FROM subscriptions WHERE expires>?`, time.Now().UnixMilli())
	if err != nil {
		e.m.mu.Unlock()
		return err
	}
	var subs []Subscription
	for rows.Next() {
		var b []byte
		var s Subscription
		if err = rows.Scan(&b); err != nil {
			break
		}
		if err = json.Unmarshal(b, &s); err != nil {
			break
		}
		if s.Owner == e.owner {
			subs = append(subs, s)
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err == nil {
		err = rowErr
	}
	if err != nil {
		e.m.mu.Unlock()
		return err
	}
	if len(subs) == 0 {
		e.m.models[ModelDots] = false
	}
	for _, t := range e.m.tasks {
		if t.State != "queued" || t.Request.Model != ModelDots {
			continue
		}
		for _, s := range subs {
			id := "evt_" + digest(s.ID+"\x00"+t.ID)
			body, _ := json.Marshal(map[string]any{"eventId": id, "name": "task.available", "timestamp": t.Created.Format(time.RFC3339Nano), "data": map[string]string{"request_id": t.ID, "queue": "default"}, "cursor": nil})
			_, err = e.m.store.db.Exec(`INSERT OR IGNORE INTO event_outbox(id,subscription_id,task_id,payload,next_attempt) VALUES(?,?,?,?,?)`, id, s.ID, t.ID, body, time.Now().UnixMilli())
			if err != nil {
				e.m.mu.Unlock()
				return err
			}
		}
	}
	var id, subID string
	var body []byte
	var attempts int
	err = e.m.store.db.QueryRow(`SELECT o.id,o.subscription_id,o.payload,o.attempts FROM event_outbox o JOIN tasks t ON t.id=o.task_id JOIN subscriptions s ON s.id=o.subscription_id WHERE o.delivered=0 AND o.next_attempt<=? AND t.state='queued' AND s.expires>? AND NOT EXISTS(SELECT 1 FROM event_outbox pending JOIN tasks pt ON pt.id=pending.task_id JOIN subscriptions ps ON ps.id=pending.subscription_id WHERE pending.delivered=1 AND pt.state='queued' AND ps.expires>?) ORDER BY o.next_attempt LIMIT 1`, time.Now().UnixMilli(), time.Now().UnixMilli(), time.Now().UnixMilli()).Scan(&id, &subID, &body, &attempts)
	e.m.mu.Unlock()
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	s, err := e.subscription(subID)
	if err != nil {
		return err
	}
	_, sendErr := e.send(ctx, s, id, body)
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	if sendErr == nil {
		_, err = e.m.store.db.Exec(`UPDATE event_outbox SET delivered=1 WHERE id=?`, id)
		return err
	}
	delay := time.Duration(1<<min(attempts, 8)) * time.Second
	_, err = e.m.store.db.Exec(`UPDATE event_outbox SET attempts=attempts+1,next_attempt=? WHERE id=?`, time.Now().Add(delay).UnixMilli(), id)
	if err != nil {
		return err
	}
	return sendErr
}
