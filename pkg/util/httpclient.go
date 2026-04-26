package util

import (
	"net"
	"net/http"
	"net/http/cookiejar"
	"time"
)

// transportConfig는 5만 개 동시 커넥션을 처리하기 위한 Transport 파라미터입니다.
//
// Go 기본값 비교:
//   MaxIdleConns:        100   → 100,000
//   MaxIdleConnsPerHost: 0(2)  → 100,000
//   IdleConnTimeout:     90s   → 유지
const (
	maxIdleConns        = 100_000
	maxIdleConnsPerHost = 100_000

	dialTimeout       = 10 * time.Second
	dialKeepAlive     = 30 * time.Second
	idleConnTimeout   = 90 * time.Second
	tlsTimeout        = 10 * time.Second
	responseHeaderTTL = 30 * time.Second

	// 에이전트별 요청 타임아웃. 대기열 폴링은 별도로 짧게 설정 가능.
	DefaultClientTimeout = 30 * time.Second
)

// NewTransport는 고동시성 시뮬레이션 전용으로 튜닝된 http.Transport를 반환합니다.
//
// 모든 에이전트가 이 Transport 인스턴스를 공유하여 커넥션 풀을 효율적으로 재사용합니다.
// http.Transport는 내부적으로 mutex로 보호되므로 고루틴 간 동시 접근이 안전합니다.
func NewTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}

	return &http.Transport{
		DialContext: dialer.DialContext,

		// 커넥션 풀 크기: 5만 에이전트 × 최대 1~2 동시 요청 수용
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,

		// 0 = 무제한. Transport가 필요 시 새 커넥션을 생성하도록 허용.
		MaxConnsPerHost: 0,

		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: responseHeaderTTL,

		// Keep-Alive를 유지하여 TCP 3-way handshake 재비용을 막습니다.
		DisableKeepAlives: false,

		// 압축 응답을 자동 해제합니다 (기본값 유지).
		DisableCompression: false,
	}
}

// NewAgentClient는 에이전트 한 명에게 발급되는 http.Client를 생성합니다.
//
// 설계 원칙:
//   - Transport: 전체 에이전트가 공유하는 단일 인스턴스를 주입받습니다.
//     → TCP 커넥션 풀을 공유하여 소켓 재사용률을 극대화합니다.
//   - CookieJar: 에이전트마다 독립 인스턴스를 생성합니다.
//     → 각자의 waiting-token 쿠키가 다른 에이전트와 혼용되지 않습니다.
func NewAgentClient(sharedTransport *http.Transport) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	return &http.Client{
		Transport: sharedTransport,
		Jar:       jar,
		Timeout:   DefaultClientTimeout,

		// 302 등 리다이렉트를 따라가지 않습니다.
		// 티켓팅 API는 리다이렉트를 사용하지 않으며,
		// 에이전트가 예기치 않은 경로로 이동하는 것을 방지합니다.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}
