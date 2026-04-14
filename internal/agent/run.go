package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// -----------------------------------------------------------------------
// 내부 전용 응답/요청 DTO
// -----------------------------------------------------------------------

type queueEntryResp struct {
	UserID   string `json:"userId"`
	Position int    `json:"position"`
}

type queueStatusResp struct {
	Position *int   `json:"position"` // null 가능 (유저가 큐에 없을 때)
	Status   string `json:"status"`   // "open" | "closed"
	Token    string `json:"token"`    // 대기 통과 시 JWT 발급
}

type getSeatsResp struct {
	Seats [][]bool `json:"seats"` // true = 예약됨, false = 빈 좌석
}

// seatCoord는 agent.go의 selectedSeat 필드 타입으로도 사용됩니다.
type seatCoord struct {
	BlockID int `json:"block_id"`
	Row     int `json:"row"`
	Col     int `json:"col"`
}

type createReservationReq struct {
	SessionID int         `json:"session_id"`
	Seats     []seatCoord `json:"seats"`
}

type createReservationResp struct {
	Rank            int         `json:"rank"`
	Seats           []seatCoord `json:"seats"`
	VirtualUserSize int         `json:"virtual_user_size"`
	ReservedAt      time.Time   `json:"reserved_at"`
}

type captchaVerifyReq struct {
	CaptchaID string `json:"captchaId"`
	UserInput string `json:"userInput"`
}

// -----------------------------------------------------------------------
// Run — 에이전트 생애 주기 진입점
// -----------------------------------------------------------------------

// Run은 에이전트 고루틴의 메인 루프입니다.
// ctx 취소(graceful shutdown) 또는 Done/Aborted 상태 도달 시 종료합니다.
func (a *Agent) Run(ctx context.Context) {
	a.startTime = time.Now()

	if a.shouldLog() {
		log.Printf("[Agent %d/%s] 시작 PatienceLimit=%s",
			a.ID, a.PersonalityType, a.PatienceLimit)
	}

	// 1. 대기열 진입
	if err := a.doEnterQueue(ctx); err != nil {
		a.CurrentState = StateAborted
		log.Printf("[Agent %d/%s] 대기열 진입 실패: %v", a.ID, a.PersonalityType, err)
		return
	}
	a.CurrentState = StateQueueing

	// 2. 상태 머신 루프
	for {
		select {
		case <-ctx.Done():
			a.CurrentState = StateAborted
			return
		default:
		}

		switch a.CurrentState {
		case StateQueueing:
			a.doQueueing(ctx)
		case StateSeatSelect:
			a.doSeatSelect(ctx)
		case StateReserving:
			a.doReserve(ctx)
		case StateDone, StateAborted:
			if a.shouldLog() {
				log.Printf("[Agent %d/%s] 종료 state=%s elapsed=%s",
					a.ID, a.PersonalityType, a.CurrentState,
					time.Since(a.startTime).Round(time.Second))
			}
			return
		}
	}
}

// -----------------------------------------------------------------------
// doEnterQueue — POST /api/queue/entries
// -----------------------------------------------------------------------

func (a *Agent) doEnterQueue(ctx context.Context) error {
	var resp queueEntryResp
	status, err := a.doJSON(ctx, http.MethodPost,
		a.config.QueueURL+"/queue/entries",
		nil, &resp)
	if err != nil {
		return fmt.Errorf("대기열 진입 요청 오류: %w", err)
	}
	if status != http.StatusCreated {
		return fmt.Errorf("대기열 진입 실패 HTTP %d", status)
	}
	// CookieJar가 waiting-token 쿠키를 자동 저장.
	// userId는 로깅/디버깅 용도로 보관.
	a.waitingToken = resp.UserID
	a.lastPosition = resp.Position
	return nil
}

// -----------------------------------------------------------------------
// doQueueing — GET /api/queue/entries/me 폴링
// -----------------------------------------------------------------------

// doQueueing은 대기 순번을 폴링하며 상태 전이를 결정합니다.
//
// 전이 조건:
//   - position==0 && token != ""   → StateSeatSelect
//   - PatienceLimit 초과            → StateAborted
//   - StagnantCount >= Threshold    → PanicMode 진입 (폴링 주기 단축)
func (a *Agent) doQueueing(ctx context.Context) {
	// PanicMode 여부에 따라 대기 간격 결정
	var wait time.Duration
	if a.PanicMode {
		wait = a.panicThinkTime()
	} else {
		wait = a.thinkTime()
	}

	select {
	case <-ctx.Done():
		a.CurrentState = StateAborted
		return
	case <-time.After(wait):
	}

	// PatienceLimit 초과 시 이탈
	if time.Since(a.startTime) >= a.PatienceLimit {
		if a.shouldLog() {
			log.Printf("[Agent %d/%s] 인내심 한계 도달 (pos=%d)",
				a.ID, a.PersonalityType, a.lastPosition)
		}
		a.CurrentState = StateAborted
		return
	}

	var resp queueStatusResp
	statusCode, err := a.doJSON(ctx, http.MethodGet,
		a.config.QueueURL+"/queue/entries/me",
		nil, &resp)
	if err != nil || statusCode != http.StatusOK {
		// 일시적 네트워크 오류는 무시 — 다음 폴링 주기에 재시도
		return
	}

	// 대기열 통과: position 0 + JWT 발급
	if resp.Token != "" && resp.Position != nil && *resp.Position == 0 {
		a.activeToken = resp.Token
		a.PanicMode = false
		a.StagnantCount = 0
		a.CurrentState = StateSeatSelect
		if a.shouldLog() {
			log.Printf("[Agent %d/%s] 대기열 통과 → SeatSelect", a.ID, a.PersonalityType)
		}
		return
	}

	// 순서 정체 감지 (Maister 대기열 심리학)
	currentPos := 0
	if resp.Position != nil {
		currentPos = *resp.Position
	}

	if currentPos > 0 && currentPos >= a.lastPosition {
		a.StagnantCount++
		if !a.PanicMode && a.StagnantCount >= a.StagnantThreshold {
			a.PanicMode = true
			if a.shouldLog() {
				log.Printf("[Agent %d/%s] PanicMode 진입 (pos=%d stagnant=%d)",
					a.ID, a.PersonalityType, currentPos, a.StagnantCount)
			}
		}
	} else if currentPos > 0 && currentPos < a.lastPosition {
		// 순서가 줄었으면 정체 카운터 리셋
		a.StagnantCount = 0
	}

	a.lastPosition = currentPos

	if a.shouldLog() {
		log.Printf("[Agent %d/%s] 대기 중 pos=%d panic=%v",
			a.ID, a.PersonalityType, currentPos, a.PanicMode)
	}
}

// -----------------------------------------------------------------------
// doSeatSelect — GET /reservations
// -----------------------------------------------------------------------

// doSeatSelect는 좌석 현황을 조회하고 빈 좌석을 랜덤으로 선택합니다.
// 선택 전 0.5~1.5s '망설임' 지연을 삽입하여 실제 유저 행동을 모사합니다.
func (a *Agent) doSeatSelect(ctx context.Context) {
	url := fmt.Sprintf("%s/reservations?session_id=%d&block_id=%d",
		a.config.BookingURL, a.SessionID, a.BlockID)

	var resp getSeatsResp
	statusCode, err := a.doJSON(ctx, http.MethodGet, url, nil, &resp)
	if err != nil || statusCode != http.StatusOK {
		log.Printf("[Agent %d/%s] 좌석 조회 실패 HTTP %d err=%v url=%s",
			a.ID, a.PersonalityType, statusCode, err, url)
		a.CurrentState = StateAborted
		return
	}

	// 빈 좌석(false) 목록 수집
	type coord struct{ row, col int }
	var available []coord
	for r, row := range resp.Seats {
		for c, taken := range row {
			if !taken {
				available = append(available, coord{r, c})
			}
		}
	}

	if len(available) == 0 {
		if a.shouldLog() {
			log.Printf("[Agent %d/%s] 빈 좌석 없음 → Aborted", a.ID, a.PersonalityType)
		}
		a.CurrentState = StateAborted
		return
	}

	// 망설임 지연 (Maister: 선택 불안)
	select {
	case <-ctx.Done():
		a.CurrentState = StateAborted
		return
	case <-time.After(a.hesitateTime()):
	}

	// 랜덤 좌석 선택 후 Reserving 전이
	picked := available[a.rng.Intn(len(available))]
	a.selectedSeat = seatCoord{
		BlockID: a.BlockID,
		Row:     picked.row,
		Col:     picked.col,
	}

	if a.shouldLog() {
		log.Printf("[Agent %d/%s] 좌석 선택 row=%d col=%d → Reserving",
			a.ID, a.PersonalityType, picked.row, picked.col)
	}
	a.CurrentState = StateReserving
}

// -----------------------------------------------------------------------
// doReserve — POST /reservations
// -----------------------------------------------------------------------

// doReserve는 캡차(선택적)를 처리한 후 예약을 확정합니다.
//
// 이선좌(HTTP 400 Conflict) 발생 시 최대 3회까지 SeatSelect로 회귀합니다.
// 3회 초과 또는 그 외 오류 → StateAborted.
func (a *Agent) doReserve(ctx context.Context) {
	if !a.config.SkipCaptcha {
		// 실패해도 예약 진행 — 서버가 캡차를 예약의 필수 조건으로 강제하지 않음
		_ = a.doCaptcha(ctx)
	}

	body, _ := json.Marshal(createReservationReq{
		SessionID: a.SessionID,
		Seats:     []seatCoord{a.selectedSeat},
	})

	var result createReservationResp
	statusCode, err := a.doJSON(ctx,
		http.MethodPost,
		a.config.BookingURL+"/reservations",
		bytes.NewReader(body),
		&result,
	)

	switch {
	case err != nil:
		log.Printf("[Agent %d/%s] 예약 요청 오류: %v", a.ID, a.PersonalityType, err)
		a.CurrentState = StateAborted

	case statusCode == http.StatusCreated:
		if a.shouldLog() {
			log.Printf("[Agent %d/%s] 예약 성공 rank=%d",
				a.ID, a.PersonalityType, result.Rank)
		}
		a.CurrentState = StateDone

	case statusCode == http.StatusBadRequest && a.conflictRetries < 3:
		// 이선좌: 다른 유저가 먼저 선점 → SeatSelect 회귀 후 재선택
		a.conflictRetries++
		if a.shouldLog() {
			log.Printf("[Agent %d/%s] 이선좌 발생 retry=%d → SeatSelect",
				a.ID, a.PersonalityType, a.conflictRetries)
		}
		a.CurrentState = StateSeatSelect

	default:
		log.Printf("[Agent %d/%s] 예약 실패 HTTP %d → Aborted", a.ID, a.PersonalityType, statusCode)
		a.CurrentState = StateAborted
	}
}

// -----------------------------------------------------------------------
// doCaptcha — GET /captcha + POST /captcha/verify
// -----------------------------------------------------------------------

// doCaptcha는 캡차 이미지를 요청하고 랜덤 값으로 검증을 시도합니다.
// SVG 이미지를 자동 해독할 수 없으므로 랜덤 6자리 숫자를 전송합니다.
// 서버는 캡차 실패 시에도 예약을 차단하지 않으므로 오류는 무시합니다.
func (a *Agent) doCaptcha(ctx context.Context) error {
	// 1. 캡차 이미지 요청 → X-Captcha-Id 헤더 획득
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.config.BookingURL+"/captcha", nil)
	if err != nil {
		return err
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	captchaID := resp.Header.Get("X-Captcha-Id")
	// SVG 본문 소진 후 닫기 — 커넥션 재사용을 위해 필수
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()

	if captchaID == "" {
		return nil
	}

	// 2. 랜덤 6자리 추측값으로 검증 요청 (서버 부하 시뮬레이션 목적)
	guess := fmt.Sprintf("%06d", a.rng.Intn(1_000_000))
	body, _ := json.Marshal(captchaVerifyReq{CaptchaID: captchaID, UserInput: guess})
	_, _ = a.doJSON(ctx, http.MethodPost,
		a.config.BookingURL+"/captcha/verify",
		bytes.NewReader(body), nil)

	return nil
}

// -----------------------------------------------------------------------
// doJSON — 범용 HTTP 요청 헬퍼
// -----------------------------------------------------------------------

// doJSON은 HTTP 요청을 수행하고 응답 본문을 JSON으로 디코딩합니다.
//
// Response.Body 처리 전략:
//
//	json.Decode 후 남은 바이트를 io.Copy(io.Discard, ...) 로 소진한 뒤 Close.
//	이 패턴은 net/http가 TCP 커넥션을 풀로 반환하도록 보장합니다.
//	Body.Close() 단독 호출은 미소진 데이터가 있을 때 커넥션을 버리고
//	새 소켓을 열기 때문에 ephemeral port 고갈로 이어집니다.
func (a *Agent) doJSON(
	ctx context.Context,
	method, url string,
	body io.Reader,
	target interface{},
) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.activeToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.activeToken)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		// 잔여 바이트를 반드시 소진 후 닫아야 커넥션이 풀로 반환됩니다.
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()
	}()

	if target != nil {
		if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
			// 빈 바디(204 등)는 디코딩 오류를 무시합니다.
			if resp.StatusCode != http.StatusNoContent {
				return resp.StatusCode, fmt.Errorf("JSON decode (HTTP %d): %w", resp.StatusCode, err)
			}
		}
	}

	return resp.StatusCode, nil
}
