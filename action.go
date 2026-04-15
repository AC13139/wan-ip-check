package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Action represents a configurable output plugin (e.g., Cloudflare DNS, Discord).
type Action struct {
	ID         int64            `json:"id"`
	Type       string           `json:"type"`
	Name       string           `json:"name"`
	Enabled    bool             `json:"enabled"`
	ConfigJSON json.RawMessage  `json:"config"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

// SourceAction links a source to an action with routing config.
type SourceAction struct {
	SourceID       int64           `json:"source_id"`
	ActionID       int64           `json:"action_id"`
	TriggerOn      string          `json:"trigger_on"` // ip_changed, ip_unchanged_daily, both
	RunOrder       int             `json:"run_order"`
	Enabled        bool            `json:"enabled"`
	// Joined fields from actions table (populated by GetLinkedActions)
	ActionType     string          `json:"action_type"`
	ActionName     string          `json:"action_name"`
	ActionConfigJSON json.RawMessage `json:"action_config"`
}

// ActionRun records the result of executing an action.
type ActionRun struct {
	ID         int64     `json:"id"`
	SourceID   int64     `json:"source_id"`
	ActionID   int64     `json:"action_id"`
	EventType  string    `json:"event_type"`
	OldIP      string    `json:"old_ip"`
	NewIP      string    `json:"new_ip"`
	Status     string    `json:"status"` // success, failed, skipped
	NeedsRetry bool      `json:"needs_retry"`
	ErrorMsg   string    `json:"error_msg"`
	ExecutedAt time.Time `json:"executed_at"`
}

// ActionRunWithType extends ActionRun with the action's type (for retry dispatch).
type ActionRunWithType struct {
	ID         int64  `json:"id"`
	SourceID   int64  `json:"source_id"`
	ActionID   int64  `json:"action_id"`
	EventType  string `json:"event_type"`
	OldIP      string `json:"old_ip"`
	NewIP      string `json:"new_ip"`
	ActionType string `json:"action_type"`
}

// IPEvent carries event data passed to action executors.
type IPEvent struct {
	SourceID   int64  `json:"source_id"`
	SourceName string `json:"source_name"`
	EventType  string `json:"event_type"` // ip_changed, ip_unchanged_daily
	OldIP      string `json:"old_ip"`
	NewIP      string `json:"new_ip"`
	Timestamp  string `json:"timestamp"` // RFC3339
}

// ActionExecutor is the interface that action type implementations must satisfy.
type ActionExecutor interface {
	// Type returns the action type identifier (e.g., "cloudflare_dns").
	Type() string

	// Execute runs the action with the given event and config.
	Execute(ctx context.Context, event IPEvent, config json.RawMessage) error

	// ValidateConfig checks if the config JSON is valid for this action type.
	ValidateConfig(config json.RawMessage) error

	// SecretFields returns the config field names that should be encrypted at rest.
	SecretFields() []string

	// DefaultConfig returns a default config template for this action type.
	DefaultConfig() json.RawMessage

	// ConfigSchema returns field definitions for Web UI form rendering.
	ConfigSchema() []ConfigField
}

// ConfigField describes a single config field for UI form rendering.
type ConfigField struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Type        string `json:"type"` // text, password, textarea, select, tags
	Required    bool   `json:"required"`
	Placeholder string `json:"placeholder"`
	HelpText    string `json:"help_text"`
	IsSecret    bool   `json:"is_secret"`
}

// ActionRegistry maps action type names to their executor implementations.
type ActionRegistry struct {
	executors map[string]ActionExecutor
}

// NewActionRegistry creates a new empty registry.
func NewActionRegistry() *ActionRegistry {
	return &ActionRegistry{executors: make(map[string]ActionExecutor)}
}

// Register adds an executor for the given action type.
func (r *ActionRegistry) Register(executor ActionExecutor) {
	r.executors[executor.Type()] = executor
}

// Get returns the executor for the given action type.
func (r *ActionRegistry) Get(actionType string) (ActionExecutor, error) {
	executor, ok := r.executors[actionType]
	if !ok {
		return nil, fmt.Errorf("unknown action type: %q", actionType)
	}
	return executor, nil
}

// Types returns all registered action type names.
func (r *ActionRegistry) Types() []string {
	types := make([]string, 0, len(r.executors))
	for t := range r.executors {
		types = append(types, t)
	}
	return types
}

const secretMask = "•••••"

// EncryptActionSecrets encrypts the secret fields in an action's config JSON.
// Fields set to the mask placeholder ("•••••") are skipped to avoid overwriting real secrets.
func EncryptActionSecrets(configJSON json.RawMessage, secretFields []string, key []byte) (json.RawMessage, error) {
	if len(secretFields) == 0 || len(key) == 0 {
		return configJSON, nil
	}

	var config map[string]interface{}
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	for _, field := range secretFields {
		val, ok := config[field]
		if !ok {
			continue
		}
		plaintext, ok := val.(string)
		if !ok || plaintext == "" || plaintext == secretMask {
			continue // skip mask placeholder — don't overwrite real secret
		}
		if isEncrypted(plaintext) {
			continue // already encrypted
		}
		encrypted, err := Encrypt(plaintext, key)
		if err != nil {
			return nil, fmt.Errorf("encrypting field %s: %w", field, err)
		}
		config[field] = "enc:" + encrypted
	}

	return json.Marshal(config)
}

// DecryptActionSecrets decrypts the secret fields in an action's config JSON.
func DecryptActionSecrets(configJSON json.RawMessage, secretFields []string, key []byte) (json.RawMessage, error) {
	if len(secretFields) == 0 || len(key) == 0 {
		return configJSON, nil
	}

	var config map[string]interface{}
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	for _, field := range secretFields {
		val, ok := config[field]
		if !ok {
			continue
		}
		encoded, ok := val.(string)
		if !ok || !isEncrypted(encoded) {
			continue
		}
		plaintext, err := Decrypt(encoded[4:], key) // strip "enc:" prefix
		if err != nil {
			return nil, fmt.Errorf("decrypting field %s: %w", field, err)
		}
		config[field] = plaintext
	}

	return json.Marshal(config)
}

func isEncrypted(s string) bool {
	return len(s) > 4 && s[:4] == "enc:"
}
