package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/viixix/neticket-agent/internal/agent"
	"github.com/viixix/neticket-agent/pkg/util"
)

func main() {
	cfg := parseFlags()

	// ----------------------------------------------------------------
	// 1. OS별 시스템 제한 적용 (FD 100,000 / Windows ephemeral port 안내)
	// ----------------------------------------------------------------
	if err := agent.ApplySystemLimits(); err != nil {
		log.Printf("[WARN] 시스템 제한 적용 실패 (권한 부족?): %v", err)
		log.Println("[WARN] 계속 실행하지만 FD 고갈이 발생할 수 있습니다.")
	}

	// ----------------------------------------------------------------
	// 2. 공유 HTTP Transport 초기화 (프로세스당 1개)
	// ----------------------------------------------------------------
	transport := util.NewTransport()

	// ----------------------------------------------------------------
	// 3. 세션/블록 자동 발견 (--auto-discover 플래그 시)
	// ----------------------------------------------------------------
	var sessionBlocks []agent.SessionBlock
	if cfg.AutoDiscover {
		discoveryClient := &http.Client{Transport: transport}
		log.Printf("[main] 세션/블록 자동 발견 중... (%s)", cfg.APIURL)
		pairs, err := agent.DiscoverSessionBlocks(context.Background(), cfg.APIURL, discoveryClient)
		if err != nil {
			log.Printf("[WARN] 자동 발견 실패: %v — CLI 값(session-id=%d, block-id=%d)으로 폴백합니다.",
				err, cfg.SessionID, cfg.BlockID)
		} else if len(pairs) == 0 {
			log.Printf("[WARN] 활성/예정 티켓팅 없음 — CLI 값(session-id=%d, block-id=%d)으로 폴백합니다.",
				cfg.SessionID, cfg.BlockID)
		} else {
			sessionBlocks = pairs
			log.Printf("[main] 발견된 (세션, 블록) 쌍: %d개", len(sessionBlocks))
			for _, sb := range sessionBlocks {
				log.Printf("         session=%d block=%d", sb.SessionID, sb.BlockID)
			}
		}
	}

	// ----------------------------------------------------------------
	// 4. Graceful Shutdown 컨텍스트
	//    SIGINT(Ctrl+C) / SIGTERM 수신 시 모든 고루틴에 ctx.Done() 전파
	//    --max-duration > 0 이면 타이머 만료 시에도 ctx 취소
	// ----------------------------------------------------------------
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.MaxDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.MaxDuration)
		defer cancel()
	}

	// ----------------------------------------------------------------
	// 5. 진행 상황 카운터 (원자적 — 고루틴 간 mutex 없이 집계)
	// ----------------------------------------------------------------
	var (
		countDone    atomic.Int64
		countAborted atomic.Int64

		// 페르소나별 카운터 [Hopeful=0, Doubtful=1, Hopeless=2]
		doneByPersona    [3]atomic.Int64
		abortedByPersona [3]atomic.Int64
	)

	// HTTP 에러 공유 카운터
	counters := &agent.SharedCounters{}

	// E2E latency (대기열 진입 → 통과) 수집
	latencyCh := make(chan time.Duration, 10000)
	var (
		latencyMu sync.Mutex
		latencies  []float64
	)
	go func() {
		for d := range latencyCh {
			latencyMu.Lock()
			latencies = append(latencies, d.Seconds())
			latencyMu.Unlock()
		}
	}()

	// ----------------------------------------------------------------
	// 6. 에이전트 풀 생성 및 Ramp-up
	//
	//    에이전트를 배치(batch)로 나눠 시작하여 부하를 점진적으로 증가시킵니다.
	//    (k6 stages / JMeter ramp-up period 와 동일한 개념)
	//
	//    batchSize = 500: 500명씩 묶어서 시작
	//    batchDelay = 100ms: 배치 간 간격 → 50,000명 전체 시작에 10초 소요
	// ----------------------------------------------------------------
	const (
		batchSize  = 500
		batchDelay = 100 * time.Millisecond
	)

	var wg sync.WaitGroup
	wg.Add(cfg.TotalAgents)

	log.Printf("[main] 에이전트 %d개 시작 (batchSize=%d, batchDelay=%s)",
		cfg.TotalAgents, batchSize, batchDelay)
	startedAt := time.Now()

	for i := 0; i < cfg.TotalAgents; i++ {
		// ctx 취소 시 남은 에이전트 생성 중단
		select {
		case <-ctx.Done():
			// 아직 Add()한 나머지 카운트를 Done()으로 소진해야 wg.Wait()가 풀립니다.
			remaining := cfg.TotalAgents - i
			for j := 0; j < remaining; j++ {
				wg.Done()
			}
			goto waitAll
		default:
		}

		// 배치 경계마다 딜레이 삽입
		if i > 0 && i%batchSize == 0 {
			select {
			case <-ctx.Done():
				remaining := cfg.TotalAgents - i
				for j := 0; j < remaining; j++ {
					wg.Done()
				}
				goto waitAll
			case <-time.After(batchDelay):
			}
		}

		// 에이전트별 독립 HTTP Client (공유 Transport + 전용 CookieJar)
		client, err := util.NewAgentClient(transport)
		if err != nil {
			log.Fatalf("[main] 에이전트 클라이언트 생성 실패: %v", err)
		}

		// 에이전트별 (세션, 블록) 랜덤 배정
		var sb agent.SessionBlock
		if len(sessionBlocks) > 0 {
			sb = sessionBlocks[i%len(sessionBlocks)]
		}
		a := agent.NewAgent(i, cfg, client, sb, counters)

		go func(a *agent.Agent) {
			defer wg.Done()
			a.Run(ctx)

			p := int(a.PersonalityType)
			switch a.CurrentState {
			case agent.StateDone:
				countDone.Add(1)
				doneByPersona[p].Add(1)
				if a.QueueLatency > 0 {
					latencyCh <- a.QueueLatency
				}
			default:
				countAborted.Add(1)
				abortedByPersona[p].Add(1)
			}
		}(a)
	}

waitAll:
	log.Printf("[main] 모든 에이전트 시작 완료 (elapsed=%s)", time.Since(startedAt).Round(time.Millisecond))

	// ----------------------------------------------------------------
	// 7. 주기적 진행 상황 리포트 (10초 간격)
	// ----------------------------------------------------------------
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				done := countDone.Load()
				aborted := countAborted.Load()
				total := int64(cfg.TotalAgents)

				latencyMu.Lock()
				cp := make([]float64, len(latencies))
				copy(cp, latencies)
				latencyMu.Unlock()

				slices.Sort(cp)
				p50, p95 := percentile(cp, 0.5), percentile(cp, 0.95)

				log.Printf("[진행] Done=%d(H=%d D=%d HL=%d) Aborted=%d(H=%d D=%d HL=%d) Running=%d / Total=%d | queue p50=%.1fs p95=%.1fs | 500=%d 502=%d 503=%d net=%d",
					done,
					doneByPersona[0].Load(), doneByPersona[1].Load(), doneByPersona[2].Load(),
					aborted,
					abortedByPersona[0].Load(), abortedByPersona[1].Load(), abortedByPersona[2].Load(),
					total-done-aborted, total,
					p50, p95,
					counters.Err500.Load(), counters.Err502.Load(), counters.Err503.Load(), counters.ErrNet.Load(),
				)
			}
		}
	}()

	// ----------------------------------------------------------------
	// 8. 전체 종료 대기
	// ----------------------------------------------------------------
	wg.Wait()

	close(latencyCh)

	elapsed := time.Since(startedAt).Round(time.Second)
	done := countDone.Load()
	aborted := countAborted.Load()

	latencyMu.Lock()
	cp := make([]float64, len(latencies))
	copy(cp, latencies)
	latencyMu.Unlock()

	slices.Sort(cp)

	log.Printf("[main] 시뮬레이션 완료 elapsed=%s", elapsed)
	log.Printf("[결과] Done=%d / Aborted=%d / Total=%d",
		done, aborted, int64(cfg.TotalAgents))
	log.Printf("[페르소나] Hopeful Done=%d Aborted=%d | Doubtful Done=%d Aborted=%d | Hopeless Done=%d Aborted=%d",
		doneByPersona[0].Load(), abortedByPersona[0].Load(),
		doneByPersona[1].Load(), abortedByPersona[1].Load(),
		doneByPersona[2].Load(), abortedByPersona[2].Load(),
	)
	log.Printf("[대기열 latency] 샘플=%d p50=%.1fs p95=%.1fs p99=%.1fs",
		len(cp),
		percentile(cp, 0.5),
		percentile(cp, 0.95),
		percentile(cp, 0.99),
	)
	log.Printf("[HTTP 에러] 500=%d 502=%d 503=%d net=%d",
		counters.Err500.Load(), counters.Err502.Load(),
		counters.Err503.Load(), counters.ErrNet.Load(),
	)
}

// percentile은 정렬된 슬라이스에서 p번째 백분위수를 반환합니다 (p: 0.0~1.0).
// 슬라이스가 비어 있으면 0을 반환합니다.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

// ----------------------------------------------------------------
// parseFlags — CLI 플래그 파싱
// ----------------------------------------------------------------

func parseFlags() *agent.Config {
	cfg := agent.DefaultConfig()

	flag.StringVar(&cfg.QueueURL, "queue-url", cfg.QueueURL,
		"대기열 서버 Base URL (예: http://localhost:3003/api)")
	flag.StringVar(&cfg.BookingURL, "booking-url", cfg.BookingURL,
		"예약 서버 Base URL (예: http://localhost:3002/api)")
	flag.IntVar(&cfg.SessionID, "session-id", cfg.SessionID,
		"시뮬레이션 대상 회차 ID")
	flag.IntVar(&cfg.BlockID, "block-id", cfg.BlockID,
		"시뮬레이션 대상 구역 ID")
	flag.IntVar(&cfg.TotalAgents, "agents", cfg.TotalAgents,
		"생성할 가상 유저(에이전트) 수")
	flag.BoolVar(&cfg.SkipCaptcha, "skip-captcha", cfg.SkipCaptcha,
		"캡차 단계 건너뛰기 (서버 측 bypass 설정과 함께 사용)")
	flag.IntVar(&cfg.LogEvery, "log-every", cfg.LogEvery,
		"로그 샘플링 간격 (ID % N == 0인 에이전트만 상태 로그 출력, 1=전체)")
	flag.StringVar(&cfg.APIURL, "api-url", cfg.APIURL,
		"API 서버 Base URL (자동 발견 시 사용, 예: https://show.neticket.site/api)")
	flag.BoolVar(&cfg.AutoDiscover, "auto-discover", cfg.AutoDiscover,
		"활성/예정 티켓팅 세션+블록을 API에서 자동 조회하여 에이전트마다 랜덤 배정")
	flag.BoolVar(&cfg.SpoofIP, "spoof-ip", cfg.SpoofIP,
		"각 에이전트에 랜덤 X-Forwarded-For 헤더 추가 (단일 머신 부하 테스트 시 per-IP rate limit 우회)")
	flag.DurationVar(&cfg.MaxDuration, "max-duration", cfg.MaxDuration,
		"테스트 최대 실행 시간 (예: 2m30s). 0이면 무제한. 티켓팅 세션 윈도우 내에서 강제 종료할 때 사용.")

	flag.Parse()
	return cfg
}
