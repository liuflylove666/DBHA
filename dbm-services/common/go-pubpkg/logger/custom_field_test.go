package logger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestCustomField(t *testing.T) {
	dir := t.TempDir()
	accessPath := filepath.Join(dir, "access.log")
	errorPath := filepath.Join(dir, "error.log")
	var ops = []TreeOption{
		{
			FileName: accessPath,
			Rpt: RotateOptions{
				MaxSize:    1,
				MaxAge:     1,
				MaxBackups: 3,
				Compress:   true,
			},
			Lef: func(level zapcore.Level) bool {
				return level <= zap.InfoLevel
			},
		},
		{
			FileName: errorPath,
			Rpt: RotateOptions{
				MaxSize:    1,
				MaxAge:     1,
				MaxBackups: 3,
				Compress:   true,
			},
			Lef: func(level zapcore.Level) bool {
				return level > zap.InfoLevel
			},
		},
	}

	logger := NewRotate(ops)
	field := &CustomField{UID: "42", NodeID: "node_id_42", IP: "127.0.0.1"}
	logger.Zap.Info("testing info", zap.Inline(field))
	logger.Zap.Warn("testing warn", zap.Inline(field))
	if err := logger.Sync(); err != nil {
		t.Fatalf("sync logs: %v", err)
	}

	for _, path := range []string{accessPath, errorPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var record map[string]any
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		for key, want := range map[string]string{"uid": "42", "node_id": "node_id_42", "ip": "127.0.0.1"} {
			if got := record[key]; got != want {
				t.Errorf("%s field %s = %v, want %s", path, key, got, want)
			}
		}
	}
}

type CustomField struct {
	UID    string
	NodeID string
	IP     string
}

func (f CustomField) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("uid", f.UID)
	enc.AddString("node_id", f.NodeID)
	enc.AddString("ip", f.IP)
	return nil
}
