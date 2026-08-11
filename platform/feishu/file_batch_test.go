package feishu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestDispatchMessageCoalescesFileBatch(t *testing.T) {
	const appID = "cli_batch_file"
	const appSecret = "secret-batch-file"
	const chatID = "oc_batch_file"
	const userID = "ou_batch_file"

	for _, count := range []int{2, 3, 4} {
		t.Run(strconv.Itoa(count)+" files", func(t *testing.T) {
			fileBytes := make(map[string][]byte, count)
			var requestMu sync.Mutex
			var requestedKeys []string
			for i := 0; i < count; i++ {
				fileBytes[fmt.Sprintf("file_%d", i)] = []byte(fmt.Sprintf("payload-%d", i))
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
					w.Header().Set("Content-Type", "application/json")
					writeJSON(t, w, map[string]any{
						"code":                0,
						"msg":                 "success",
						"expire":              7200,
						"tenant_access_token": "tenant-token",
					})
				case strings.Contains(r.URL.Path, "/resources/"):
					key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					data, ok := fileBytes[key]
					if !ok {
						t.Fatalf("unexpected file key %q", key)
					}
					requestMu.Lock()
					requestedKeys = append(requestedKeys, key)
					requestMu.Unlock()
					w.Header().Set("Content-Type", "application/octet-stream")
					if _, err := w.Write(data); err != nil {
						t.Fatalf("write file: %v", err)
					}
				default:
					w.Header().Set("Content-Type", "application/json")
					writeJSON(t, w, map[string]any{"code": 0, "msg": "success", "data": map[string]any{}})
				}
			}))
			defer srv.Close()

			received := make(chan *core.Message, count+1)
			p := &Platform{
				platformName:    "feishu",
				domain:          srv.URL,
				appID:           appID,
				appSecret:       appSecret,
				client:          lark.NewClient(appID, appSecret, lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())),
				handler:         func(_ core.Platform, msg *core.Message) { received <- msg },
				fileBatch:       make(map[string]*fileBatchEntry),
				fileBatchWindow: 20 * time.Millisecond,
			}

			sessionKey := "feishu:" + chatID + ":" + userID
			// Deliver events newest-first to reproduce the asynchronous inversion
			// that previously let the watermark discard the oldest file.
			for i := count - 1; i >= 0; i-- {
				msgID := fmt.Sprintf("om_file_%d", i)
				content := fmt.Sprintf(`{"file_key":"file_%d","file_name":"doc-%d.txt"}`, i, i)
				p.dispatchMessage(
					context.Background(), "file", content, nil,
					msgID, sessionKey, userID, chatID,
					replyContext{messageID: msgID, chatID: chatID, sessionKey: sessionKey},
					"", int64(1710000000000)+int64(i),
				)
			}

			var msg *core.Message
			select {
			case msg = <-received:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for file batch")
			}

			if len(msg.Files) != count {
				t.Fatalf("merged message has %d files, want %d", len(msg.Files), count)
			}
			for i, file := range msg.Files {
				wantName := fmt.Sprintf("doc-%d.txt", i)
				wantData := fmt.Sprintf("payload-%d", i)
				if file.FileName != wantName {
					t.Errorf("Files[%d].FileName = %q, want %q", i, file.FileName, wantName)
				}
				if string(file.Data) != wantData {
					t.Errorf("Files[%d].Data = %q, want %q", i, file.Data, wantData)
				}
			}
			if wantID := fmt.Sprintf("om_file_%d", count-1); msg.MessageID != wantID {
				t.Errorf("MessageID = %q, want newest %q", msg.MessageID, wantID)
			}
			if wantTime := int64(1710000000000) + int64(count-1); msg.UserMessageTimeMs != wantTime {
				t.Errorf("UserMessageTimeMs = %d, want newest %d", msg.UserMessageTimeMs, wantTime)
			}

			requestMu.Lock()
			gotRequested := append([]string(nil), requestedKeys...)
			requestMu.Unlock()
			if len(gotRequested) != count {
				t.Fatalf("resource requests = %d, want %d", len(gotRequested), count)
			}
			for i, key := range gotRequested {
				if want := fmt.Sprintf("file_%d", i); key != want {
					t.Errorf("resource request[%d] = %q, want create_time order %q", i, key, want)
				}
			}

			select {
			case extra := <-received:
				t.Fatalf("unexpected extra message %q with %d files", extra.MessageID, len(extra.Files))
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestNewPlatformFileBatchWindow(t *testing.T) {
	p, err := newPlatform("feishu", lark.FeishuBaseUrl, map[string]any{
		"app_id":     "cli_test",
		"app_secret": "secret",
	})
	if err != nil {
		t.Fatalf("newPlatform error: %v", err)
	}
	fp := extractBasePlatform(p)
	if fp == nil {
		t.Fatal("expected *Platform or *interactivePlatform")
	}
	if fp.fileBatchWindow != defaultFileBatchWindow {
		t.Errorf("default fileBatchWindow = %v, want %v", fp.fileBatchWindow, defaultFileBatchWindow)
	}

	p, err = newPlatform("feishu", lark.FeishuBaseUrl, map[string]any{
		"app_id":               "cli_test",
		"app_secret":           "secret",
		"file_batch_window_ms": int64(1200),
	})
	if err != nil {
		t.Fatalf("newPlatform with custom file window error: %v", err)
	}
	fp = extractBasePlatform(p)
	if want := 1200 * time.Millisecond; fp.fileBatchWindow != want {
		t.Errorf("custom fileBatchWindow = %v, want %v", fp.fileBatchWindow, want)
	}

	p, err = newPlatform("feishu", lark.FeishuBaseUrl, map[string]any{
		"app_id":               "cli_test",
		"app_secret":           "secret",
		"file_batch_window_ms": 0,
	})
	if err != nil {
		t.Fatalf("newPlatform with zero file window error: %v", err)
	}
	fp = extractBasePlatform(p)
	if fp.fileBatchDelay() != defaultFileBatchWindow {
		t.Errorf("fileBatchDelay() with zero field = %v, want %v", fp.fileBatchDelay(), defaultFileBatchWindow)
	}

	if _, err := newPlatform("feishu", lark.FeishuBaseUrl, map[string]any{
		"app_id":               "cli_test",
		"app_secret":           "secret",
		"file_batch_window_ms": -1,
	}); err == nil {
		t.Error("expected error for negative file_batch_window_ms, got nil")
	}
	if _, err := newPlatform("feishu", lark.FeishuBaseUrl, map[string]any{
		"app_id":               "cli_test",
		"app_secret":           "secret",
		"file_batch_window_ms": "200",
	}); err == nil {
		t.Error("expected error for string file_batch_window_ms, got nil")
	}
}

func TestDispatchMessageSingleFileRegression(t *testing.T) {
	const appID = "cli_single_file"
	const appSecret = "secret-single-file"
	const fileKey = "file_single"
	fileData := []byte("single-payload")

	srv := newFileBatchTestServer(t, map[string][]byte{fileKey: fileData}, "")
	defer srv.Close()

	got := make(chan *core.Message, 1)
	p := newFileBatchTestPlatform(srv, appID, appSecret, func(_ core.Platform, msg *core.Message) {
		got <- msg
	})

	p.dispatchMessage(
		context.Background(), "file",
		`{"file_key":"file_single","file_name":"single.txt"}`,
		nil, "om_single_file", "feishu:oc_single_file:ou_user", "ou_user", "oc_single_file",
		replyContext{messageID: "om_single_file", chatID: "oc_single_file", sessionKey: "feishu:oc_single_file:ou_user"},
		"", 1710000000000,
	)

	select {
	case msg := <-got:
		if len(msg.Files) != 1 {
			t.Fatalf("len(Files) = %d, want 1", len(msg.Files))
		}
		if msg.Files[0].FileName != "single.txt" || string(msg.Files[0].Data) != string(fileData) {
			t.Fatalf("single file = (%q, %q), want (%q, %q)", msg.Files[0].FileName, msg.Files[0].Data, "single.txt", fileData)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for single file")
	}
}

func TestDispatchMessageQuotedFileNotBatched(t *testing.T) {
	const appID = "cli_quoted_file"
	const appSecret = "secret-quoted-file"
	const parentMessageID = "om_parent_file"
	const fileKey = "file_quoted"

	srv := newFileBatchTestServer(t, map[string][]byte{fileKey: []byte("quoted-payload")}, parentMessageID)
	defer srv.Close()

	got := make(chan *core.Message, 1)
	p := newFileBatchTestPlatform(srv, appID, appSecret, func(_ core.Platform, msg *core.Message) {
		got <- msg
	})

	p.dispatchMessage(
		context.Background(), "file",
		`{"file_key":"file_quoted","file_name":"quoted.txt"}`,
		nil, "om_quoted_file", "feishu:oc_quoted_file:ou_user", "ou_user", "oc_quoted_file",
		replyContext{messageID: "om_quoted_file", chatID: "oc_quoted_file", sessionKey: "feishu:oc_quoted_file:ou_user"},
		parentMessageID, 1710000000000,
	)

	p.fileBatchMu.Lock()
	batchSize := len(p.fileBatch)
	p.fileBatchMu.Unlock()
	if batchSize != 0 {
		t.Fatalf("quoted file entered batch; fileBatch size = %d", batchSize)
	}

	select {
	case msg := <-got:
		if len(msg.Files) != 1 {
			t.Fatalf("quoted file path: len(Files) = %d, want 1", len(msg.Files))
		}
		if msg.MessageID != "om_quoted_file" {
			t.Fatalf("MessageID = %q, want %q", msg.MessageID, "om_quoted_file")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for quoted file")
	}
}

func TestFlushFileBatchesStopsPendingTimers(t *testing.T) {
	const appID = "cli_flush_file"
	const appSecret = "secret-flush-file"
	const fileKey = "file_flush"

	srv := newFileBatchTestServer(t, map[string][]byte{fileKey: []byte("flush-payload")}, "")
	defer srv.Close()

	received := make(chan *core.Message, 2)
	p := newFileBatchTestPlatform(srv, appID, appSecret, func(_ core.Platform, msg *core.Message) {
		received <- msg
	})
	p.fileBatchWindow = 5 * time.Second
	sessionKey := "feishu:oc_flush_file:ou_user"
	p.bufferFile(sessionKey, &fileBatchEntry{
		sessionKey: sessionKey,
		userID:     "ou_user",
		items: []fileBatchItem{{
			messageID:    "om_flush_file",
			fileKey:      fileKey,
			fileName:     "flush.txt",
			rctx:         replyContext{messageID: "om_flush_file", chatID: "oc_flush_file", sessionKey: sessionKey},
			createTimeMs: 1710000000000,
		}},
	})

	p.flushFileBatches()

	p.fileBatchMu.Lock()
	batchSize := len(p.fileBatch)
	p.fileBatchMu.Unlock()
	if batchSize != 0 {
		t.Fatalf("fileBatch size = %d after flush, want 0", batchSize)
	}

	select {
	case msg := <-received:
		if len(msg.Files) != 1 || msg.Files[0].FileName != "flush.txt" {
			t.Fatalf("flushed files = %+v, want flush.txt", msg.Files)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for flushed file batch")
	}

	select {
	case extra := <-received:
		t.Fatalf("unexpected extra message after flush: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func newFileBatchTestPlatform(srv *httptest.Server, appID, appSecret string, handler core.MessageHandler) *Platform {
	return &Platform{
		platformName:    "feishu",
		domain:          srv.URL,
		appID:           appID,
		appSecret:       appSecret,
		client:          lark.NewClient(appID, appSecret, lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())),
		handler:         handler,
		fileBatch:       make(map[string]*fileBatchEntry),
		fileBatchWindow: 20 * time.Millisecond,
	}
}

func newFileBatchTestServer(t *testing.T, files map[string][]byte, parentMessageID string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "success",
				"expire":              7200,
				"tenant_access_token": "tenant-token",
			})
		case parentMessageID != "" && r.URL.Path == "/open-apis/im/v1/messages/"+parentMessageID:
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{
				"code": 0,
				"msg":  "success",
				"data": map[string]any{"items": []map[string]any{{
					"msg_type":  "text",
					"parent_id": "",
					"sender":    map[string]any{"id": "", "sender_type": "user"},
					"body":      map[string]any{"content": `{"text":"请处理附件"}`},
				}}},
			})
		case strings.Contains(r.URL.Path, "/resources/"):
			key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			data, ok := files[key]
			if !ok {
				t.Fatalf("unexpected file key %q", key)
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			if _, err := w.Write(data); err != nil {
				t.Fatalf("write file: %v", err)
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{"code": 0, "msg": "success", "data": map[string]any{}})
		}
	}))
}
