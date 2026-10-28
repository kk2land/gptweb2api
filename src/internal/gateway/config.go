package gateway

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	sdkconfig "github.com/openai/tunnel-client/pkg/config"
)

type Config struct {
	Server struct {
		Listen string `toml:"listen"`
		APIKey string `toml:"api_key"`
	} `toml:"server"`
	Tunnel struct {
		ID     string `toml:"id"`
		APIKey string `toml:"api_key"`
	} `toml:"tunnel"`
	Tasks struct {
		MaxConcurrency int    `toml:"max_concurrency"`
		QueueCapacity  int    `toml:"queue_capacity"`
		Timeout        string `toml:"timeout"`
	} `toml:"tasks"`
	Ego struct {
		SpaceName string `toml:"space_name"`
	} `toml:"ego"`
	Dots struct {
		Enabled bool `toml:"enabled"`
	} `toml:"dots"`
	Log struct {
		MaxSizeMB  int `toml:"max_size_mb"`
		MaxBackups int `toml:"max_backups"`
	} `toml:"log"`
	TaskTimeout time.Duration `toml:"-"`
}

func LoadConfig(root string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(root, "config", "mcp-gateway.toml"))
	if err != nil {
		return c, fmt.Errorf("read config/mcp-gateway.toml: %w", err)
	}
	md, err := toml.Decode(string(b), &c)
	if err != nil {
		// Parser errors can contain the input line, including a secret.
		return c, errors.New("invalid TOML in config/mcp-gateway.toml (check field types and syntax)")
	}
	if len(md.Undecoded()) != 0 {
		return c, errors.New("unknown configuration field")
	}
	for _, key := range []string{"server.listen", "server.api_key", "tunnel.id", "tunnel.api_key", "tasks.max_concurrency", "tasks.queue_capacity", "tasks.timeout", "dots.enabled", "log.max_size_mb", "log.max_backups"} {
		if !md.IsDefined(strings.Split(key, ".")...) {
			return c, fmt.Errorf("missing configuration field %s", key)
		}
	}
	host, port, err := net.SplitHostPort(c.Server.Listen)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || port == "0" || port == "" {
		return c, errors.New("server.listen must use a loopback IP and a nonzero port")
	}
	if portNumber, err := strconv.Atoi(port); err != nil || portNumber < 1 || portNumber > 65535 {
		return c, errors.New("server.listen contains an invalid TCP port")
	}
	for _, field := range []struct{ name, value string }{{"server.api_key", c.Server.APIKey}, {"tunnel.id", c.Tunnel.ID}, {"tunnel.api_key", c.Tunnel.APIKey}} {
		v := strings.TrimSpace(field.value)
		if v == "" || strings.Contains(strings.ToUpper(v), "REPLACE_") || strings.Contains(v, "<") || v != field.value || strings.ContainsAny(v, "\r\n") {
			return c, fmt.Errorf("%s must contain an actual credential/identity", field.name)
		}
	}
	if c.Server.APIKey == c.Tunnel.APIKey {
		return c, errors.New("server.api_key and tunnel.api_key must be different credentials")
	}
	if sdkconfig.ValidateTunnelID(c.Tunnel.ID) != nil {
		return c, errors.New("tunnel.id is not a valid OpenAI Tunnel identifier")
	}
	if c.Tasks.MaxConcurrency <= 0 || c.Tasks.QueueCapacity <= 0 || c.Log.MaxSizeMB <= 0 || c.Log.MaxBackups <= 0 {
		return c, errors.New("task and log limits must be positive")
	}
	if c.Log.MaxSizeMB > 1<<20 || c.Log.MaxBackups > 10000 || c.Tasks.MaxConcurrency > 1024 || c.Tasks.QueueCapacity > 100000 {
		return c, errors.New("task or log limit exceeds implementation capacity")
	}
	c.Ego.SpaceName = strings.TrimSpace(c.Ego.SpaceName)
	if !utf8.ValidString(c.Ego.SpaceName) || utf8.RuneCountInString(c.Ego.SpaceName) > 128 || strings.ContainsFunc(c.Ego.SpaceName, unicode.IsControl) {
		return c, errors.New("ego.space_name must be at most 128 characters without control characters")
	}
	c.TaskTimeout, err = time.ParseDuration(c.Tasks.Timeout)
	if err != nil || c.TaskTimeout <= 0 {
		return c, errors.New("tasks.timeout must be a positive Go duration, for example 30m")
	}
	return c, nil
}

func PrepareRuntime(root string) error {
	for _, path := range []string{"runtime", "runtime/logs", "runtime/data", "runtime/smoke"} {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(p, 0700); err != nil {
			return err
		}
		if err := os.Chmod(p, 0700); err != nil {
			return err
		}
	}
	return nil
}
