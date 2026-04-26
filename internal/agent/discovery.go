package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"time"
)

// SessionBlock은 에이전트 한 명이 대상으로 삼을 (회차, 구역) 쌍입니다.
type SessionBlock struct {
	SessionID int
	BlockID   int
}

const (
	// activeWindow: 티켓팅이 시작된 지 이 시간 이내면 "진행 중"으로 간주합니다.
	// 5분 주기 티켓팅 기준으로 5분이면 충분합니다.
	activeWindow = 5 * time.Minute

	// sameRoundWindow: 가장 가까운 예정 티켓팅과 이 시간 차이 이내인 공연은
	// 같은 회차로 묶어 함께 발견합니다 (동시간대 여러 공연 대응).
	sameRoundWindow = 5 * time.Minute
)

// DiscoverSessionBlocks는 "현재 진행 중이거나 가장 가까운 예정" 티켓팅의
// (세션ID, 블록ID) 목록을 반환합니다.
//
// 선택 우선순위:
//  1. ticketing_date <= now && now - ticketing_date <= 60분 → 진행 중인 티켓팅
//  2. 위 조건 없으면 → ticketing_date가 now에 가장 가까운 예정 티켓팅
//     (같은 시간대 ±5분 이내 공연은 모두 포함)
//
// 호출 순서:
//  1. GET /performances               → 공연 목록
//  2. GET /performances/:id/sessions  → 회차 목록
//  3. GET /sessions/:id/block-grades  → 구역 목록
func DiscoverSessionBlocks(ctx context.Context, apiURL string, client *http.Client) ([]SessionBlock, error) {
	performances, err := fetchPerformances(ctx, apiURL, client)
	if err != nil {
		return nil, fmt.Errorf("공연 목록 조회 실패: %w", err)
	}

	selected := selectNearestPerformances(performances)
	if len(selected) == 0 {
		return nil, nil
	}

	var pairs []SessionBlock
	for _, perf := range selected {
		sessions, err := fetchSessions(ctx, apiURL, perf.PerformanceID, client)
		if err != nil {
			log.Printf("[Discovery] 공연 %d 회차 조회 실패: %v", perf.PerformanceID, err)
			continue
		}

		for _, sess := range sessions {
			blockIDs, err := fetchBlockIDs(ctx, apiURL, sess.ID, client)
			if err != nil || len(blockIDs) == 0 {
				log.Printf("[Discovery] 세션 %d 구역 조회 실패 또는 구역 없음: %v", sess.ID, err)
				continue
			}
			for _, blockID := range blockIDs {
				pairs = append(pairs, SessionBlock{SessionID: sess.ID, BlockID: blockID})
			}
		}
	}

	return pairs, nil
}

// selectNearestPerformances는 공연 목록에서 "지금과 가장 가까운" 티켓팅 공연을 선택합니다.
func selectNearestPerformances(performances []apiPerformance) []apiPerformance {
	now := time.Now()

	// 1단계: 진행 중인 티켓팅 (ticketing_date <= now, 60분 이내 시작)
	var active []apiPerformance
	for _, p := range performances {
		t, err := time.Parse(time.RFC3339, p.TicketingDate)
		if err != nil {
			continue
		}
		elapsed := now.Sub(t)
		if elapsed >= 0 && elapsed <= activeWindow {
			active = append(active, p)
		}
	}
	if len(active) > 0 {
		log.Printf("[Discovery] 진행 중인 티켓팅 %d개 발견", len(active))
		return active
	}

	// 2단계: 가장 가까운 예정 티켓팅 탐색
	minDist := time.Duration(math.MaxInt64)
	for _, p := range performances {
		t, err := time.Parse(time.RFC3339, p.TicketingDate)
		if err != nil {
			continue
		}
		if d := t.Sub(now); d > 0 && d < minDist {
			minDist = d
		}
	}
	if minDist == time.Duration(math.MaxInt64) {
		return nil // 예정된 티켓팅 없음
	}

	// 가장 가까운 시각 ±sameRoundWindow 이내 공연 모두 포함
	var upcoming []apiPerformance
	for _, p := range performances {
		t, err := time.Parse(time.RFC3339, p.TicketingDate)
		if err != nil {
			continue
		}
		d := t.Sub(now)
		if d > 0 && d <= minDist+sameRoundWindow {
			upcoming = append(upcoming, p)
		}
	}

	// 가장 가까운 것 먼저 정렬하여 로그 출력
	sort.Slice(upcoming, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, upcoming[i].TicketingDate)
		tj, _ := time.Parse(time.RFC3339, upcoming[j].TicketingDate)
		return ti.Before(tj)
	})
	if len(upcoming) > 0 {
		t, _ := time.Parse(time.RFC3339, upcoming[0].TicketingDate)
		log.Printf("[Discovery] 예정 티켓팅 %d개 발견 (가장 가까운 시작: %s, %s 후)",
			len(upcoming), t.Format("15:04:05"), time.Until(t).Round(time.Second))
	}
	return upcoming
}

// -----------------------------------------------------------------------
// 내부 API 호출 함수
// -----------------------------------------------------------------------

type apiPerformance struct {
	PerformanceID int    `json:"performance_id"`
	TicketingDate string `json:"ticketing_date"`
}

type apiPerformancesResp struct {
	Performances []apiPerformance `json:"performances"`
}

type apiSession struct {
	ID      int `json:"id"`
	VenueID int `json:"venueId"`
}

type apiBlockGrade struct {
	BlockID int `json:"blockId"`
}

func fetchPerformances(ctx context.Context, apiURL string, client *http.Client) ([]apiPerformance, error) {
	var resp apiPerformancesResp
	// ticketing_after를 충분히 과거로 지정하지 않으면 서버가 기본값으로
	// 현재 시각을 설정해 이미 시작된 공연을 응답에서 제외합니다.
	// activeWindow(5분) 이내 시작된 공연까지 포함하도록 여유를 둡니다.
	pastCutoff := time.Now().Add(-activeWindow).UTC().Format(time.RFC3339)
	url := fmt.Sprintf("%s/performances?ticketing_after=%s", apiURL, pastCutoff)
	if err := getJSON(ctx, client, url, &resp); err != nil {
		return nil, err
	}
	return resp.Performances, nil
}

func fetchSessions(ctx context.Context, apiURL string, performanceID int, client *http.Client) ([]apiSession, error) {
	var resp []apiSession
	url := fmt.Sprintf("%s/performances/%d/sessions", apiURL, performanceID)
	if err := getJSON(ctx, client, url, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func fetchBlockIDs(ctx context.Context, apiURL string, sessionID int, client *http.Client) ([]int, error) {
	var grades []apiBlockGrade
	url := fmt.Sprintf("%s/sessions/%d/block-grades", apiURL, sessionID)
	if err := getJSON(ctx, client, url, &grades); err != nil {
		return nil, err
	}

	ids := make([]int, 0, len(grades))
	for _, g := range grades {
		ids = append(ids, g.BlockID)
	}
	return ids, nil
}

// getJSON은 GET 요청을 보내고 응답을 JSON으로 디코딩합니다.
// io.Copy(io.Discard) 로 잔여 바이트를 소진해 커넥션을 재사용합니다.
func getJSON(ctx context.Context, client *http.Client, url string, target interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(target)
}
