package agent

import "sync/atomic"

// SharedCounters는 모든 에이전트 고루틴이 공유하는 원자적 카운터입니다.
// main에서 단일 인스턴스를 생성해 각 에이전트에 포인터로 주입합니다.
type SharedCounters struct {
	Err500 atomic.Int64 // NestJS 내부 에러
	Err502 atomic.Int64 // nginx → NestJS 응답 없음 (Node.js 과부하)
	Err503 atomic.Int64 // 서비스 불가 (Redis 연결 실패, 대기열 닫힘)
	ErrNet atomic.Int64 // 네트워크 에러 (connection refused, timeout)
}
