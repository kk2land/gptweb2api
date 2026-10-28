package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const (
	ModelGPT6 = "web/gpt-6"
	ModelSol  = "web/gpt-5.6-sol"
	ModelDots = "dots"
)

type Fault struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	HTTP    int    `json:"-"`
}

func (f *Fault) Error() string               { return f.Code + ": " + f.Message }
func fault(code, msg string, http int) error { return &Fault{code, msg, http} }

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type Options struct {
	Workdir   string `json:"workdir,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	ProjectID string `json:"chatgpt_project_id,omitempty"`
}
type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream,omitempty"`
	Options  Options   `json:"x_gptweb2api"`
}
type Task struct {
	ID              string    `json:"id"`
	State           string    `json:"state"`
	Request         Request   `json:"request"`
	Created         time.Time `json:"created_at"`
	Deadline        time.Time `json:"deadline"`
	Updated         time.Time `json:"updated_at"`
	Answer          string    `json:"final_answer,omitempty"`
	Error           *Fault    `json:"error,omitempty"`
	TokenHash       string    `json:"-"`
	CompletionHash  string    `json:"-"`
	CompletedAt     time.Time `json:"-"`
	Started         bool      `json:"-"`
	Binding         Binding   `json:"-"`
	ScriptHash      string    `json:"-"`
	ActiveTools     int       `json:"-"`
	LeaseUntil      time.Time `json:"-"`
	Calibration     string    `json:"-"`
	DotSubscription string    `json:"-"`
	DirectoryID     string    `json:"-"`
}

func (t Task) Terminal() bool {
	switch t.State {
	case "completed", "failed", "cancelled", "expired":
		return true
	}
	return false
}

type Binding struct {
	SpaceID        int    `json:"space_id"`
	PageID         string `json:"page_id"`
	ConversationID string `json:"conversation_id"`
	ProfileID      string `json:"profile_id"`
}
type Session struct {
	ID            string    `json:"id"`
	Model         string    `json:"model"`
	Workdir       string    `json:"workdir"`
	ProjectID     string    `json:"project_id"`
	History       []Message `json:"history"`
	Binding       Binding   `json:"binding"`
	Valid         bool      `json:"valid"`
	LastRequestID string    `json:"last_request_id"`
	DirectoryID   string    `json:"directory_id"`
}
type Completion struct {
	Status string `json:"status"`
	Answer string `json:"final_answer"`
	Error  *Fault `json:"error,omitempty"`
}
type Credentials struct {
	RequestID string `json:"request_id"`
	Token     string `json:"turn_token"`
}

func randomID(prefix string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("system random source: %v", err))
	}
	return prefix + hex.EncodeToString(b)
}
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func cloneTask(t *Task) Task {
	b, _ := json.Marshal(t)
	var out Task
	_ = json.Unmarshal(b, &out)
	return out
}
