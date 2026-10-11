package lane

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAtomicFlagConcurrent is the race-detector guard for the probe round
// guard: ProbeRound is reachable from the probe ticker, the egress watcher,
// /admin reprobe and the rotate hook all at once. Run with -race.
func TestAtomicFlagConcurrent(t *testing.T) {
	var f atomicFlag
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5000; j++ {
				f.Set(true)
				_ = f.Get()
				f.Set(false)
			}
		}()
	}
	wg.Wait()
}

// TestClassifyModelRefusalUnder401 pins the classification-order fix. The
// gateway answers an unknown model with 401 + a body that names the model;
// reading that as a credential problem makes the client believe its API key is
// wrong and stops failover from answering with a healthy model.
func TestClassifyModelRefusalUnder401(t *testing.T) {
	payload := `{"type":"error","error":{"type":"ModelError","message":"Model nope-free is not supported"}}`
	e := ClassifyFailure(401, payload, 0)
	if e.Code != CodeServer || !e.Unavailable {
		t.Fatalf("model refusal under 401: code=%s unavailable=%v, want %s/true",
			e.Code, e.Unavailable, CodeServer)
	}
	if !switchableFailure(e) {
		t.Fatal("a model-specific refusal must stay switchable so failover can try another model")
	}

	// a genuine credential refusal must still be classified as one
	if got := ClassifyFailure(401, `{"error":"invalid api key"}`, 0); got.Code != CodeCredential {
		t.Fatalf("plain 401 classified as %s, want %s", got.Code, CodeCredential)
	}
	// and the region gate still wins over everything
	if got := ClassifyFailure(403, `{"error":"not available in your country"}`, 0); got.Code != CodeRegion {
		t.Fatalf("region payload classified as %s, want %s", got.Code, CodeRegion)
	}
}

// TestTestOneReadsTimeoutUnderLock is the race-detector guard for Configure
// (a settings save) overlapping a proxy refresh. Run with -race.
func TestTestOneReadsTimeoutUnderLock(t *testing.T) {
	p := NewProxyPool(nil)
	var workers, configurer sync.WaitGroup
	stop := make(chan struct{})

	configurer.Add(1)
	go func() {
		defer configurer.Done()
		for {
			select {
			case <-stop:
				return
			default:
				p.Configure(nil, 0, 0, 6, true)
			}
		}
	}()

	workers.Add(1)
	go func() {
		defer workers.Done()
		for i := 0; i < 50; i++ {
			// a dial that fails immediately: the point is the unlocked read,
			// not the network
			p.testOne(context.Background(), "http://127.0.0.1:1", "127.0.0.1:1", "http", "", true)
		}
	}()

	workers.Wait()
	close(stop)
	configurer.Wait()
}

// TestNormalizeProxyRejectsBadHosts pins the host-validation fix: the old guard
// tested the composed "host:port" string, which always contains a colon, so it
// never rejected anything and junk candidates were probed every round.
func TestNormalizeProxyRejectsBadHosts(t *testing.T) {
	for _, in := range []string{"1.2.3:8080", "999.999.999.999:80", "abc:8080", "0.0.0.0.0:80", ".:8080"} {
		if u, _, _, ok := normalizeProxy(in, "http"); ok {
			t.Errorf("normalizeProxy(%q) accepted it as %q; want rejection", in, u)
		}
	}
	for _, in := range []string{"1.2.3.4:8080", "127.0.0.1:1080", "proxy.example.com:3128", "localhost:8080"} {
		if _, _, _, ok := normalizeProxy(in, "http"); !ok {
			t.Errorf("normalizeProxy(%q) rejected a valid address", in)
		}
	}
}

// TestAnthropicUsageNotDoubleCounted pins the usage fix: Anthropic's terminal
// message_delta repeats the *cumulative* output count, so folding it in as an
// increment inflated every streamed turn by the seed message_start sent.
func TestAnthropicUsageNotDoubleCounted(t *testing.T) {
	frames := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":220,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":24}}`,
		`{"type":"message_stop"}`,
	}
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n")
	}
	u, err := readSSE(strings.NewReader(b.String()), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("no usage parsed")
	}
	if u.Input != 220 {
		t.Errorf("input = %d, want 220", u.Input)
	}
	if u.Output != 24 {
		t.Errorf("output = %d, want 24 (25 means message_delta was merged as an increment)", u.Output)
	}
}

// TestChatUsageKeepsSumming makes sure the Anthropic special case did not break
// the chat wire, where non-terminal usage frames must still accumulate.
func TestChatUsageKeepsSumming(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":3}}`,
	}
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n")
	}
	u, err := readSSE(strings.NewReader(b.String()), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("no usage parsed")
	}
	if u.Input != 10 || u.Output != 5 {
		t.Errorf("chat usage = %d/%d, want 10/5", u.Input, u.Output)
	}
}

// TestStreamReaderSendUnblocksOnClose pins the leak fix: the pump goroutine
// used to park forever on `s.ch <-` once the consumer stopped reading (an idle
// timeout, a cancelled request), leaking one goroutine per abandoned stream.
func TestStreamReaderSendUnblocksOnClose(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	s := newStreamReader(context.Background(), pr, 0)
	defer s.Close()

	// fill the buffer so a further send can only complete through Close()
	for i := 0; i < cap(s.ch); i++ {
		if !s.send(streamChunk{data: []byte("x")}) {
			t.Fatalf("unexpected early stop at chunk %d", i)
		}
	}
	s.Close()

	done := make(chan bool, 1)
	go func() { done <- s.send(streamChunk{data: []byte("x")}) }()
	select {
	case ok := <-done:
		if ok {
			t.Error("send() succeeded after Close with a full buffer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send() blocked after Close — the pump goroutine would leak")
	}
}
