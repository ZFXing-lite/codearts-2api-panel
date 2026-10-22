package api

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// 上游 TCP 活着但不吐字节时，看门狗必须把读打断并报 errSSEIdleTimeout。
// 这条测试防的是历史缺陷：靠 SetReadDeadline 类型断言实现的超时，
// 对 *http.bodyEOFSignal 永远断言失败，整条超时逻辑是死代码。
func TestIdleTimeoutReaderFiresOnStalledBody(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	r := newIdleTimeoutReader(pr, 80*time.Millisecond)
	buf := make([]byte, 8)
	start := time.Now()
	_, err := r.Read(buf)
	if !errors.Is(err, errSSEIdleTimeout) {
		t.Fatalf("expected idle timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("idle timeout too slow: %s", elapsed)
	}
}

// 有数据时必须原样读出，不能被看门狗误杀。
func TestIdleTimeoutReaderPassesData(t *testing.T) {
	r := newIdleTimeoutReader(strings.NewReader("data: hi\n\n"), time.Second)
	buf := make([]byte, 16)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := string(buf[:n]); !strings.Contains(got, "data:") {
		t.Fatalf("unexpected payload %q", got)
	}
}
