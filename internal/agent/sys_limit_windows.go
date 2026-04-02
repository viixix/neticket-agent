//go:build windows

package agent

import "fmt"

// ApplySystemLimits는 Windows 환경에서 시스템 제한을 점검하고
// 필요한 조치를 안내합니다.
//
// Windows의 Winsock은 POSIX RLIMIT_NOFILE 개념이 없으므로
// 파일 디스크립터 한계 자체는 설정하지 않습니다.
// 대신 5만 개 동시 연결 시 발생할 수 있는 ephemeral port 고갈을
// 방지하기 위한 레지스트리/netsh 설정을 안내합니다.
//
// 권장 조치 (관리자 권한 CMD):
//
//	netsh int ipv4 set dynamicport tcp start=1025 num=64511
//	netsh int ipv6 set dynamicport tcp start=1025 num=64511
//
// 이 설정은 동적 포트 범위를 ~64K로 확장하여 TIME_WAIT 누적으로 인한
// 포트 고갈을 크게 완화합니다. 재부팅 후에도 유지됩니다.
func ApplySystemLimits() error {
	fmt.Println("[INFO] Windows: RLIMIT_NOFILE 설정은 불필요합니다 (Winsock 자체 관리).")
	fmt.Println("[WARN] 5만 개 동시 연결 시 ephemeral port 고갈 위험이 있습니다.")
	fmt.Println("[WARN] 아래 명령어를 관리자 CMD에서 실행하세요 (1회, 재부팅 후 유지):")
	fmt.Println("         netsh int ipv4 set dynamicport tcp start=1025 num=64511")
	fmt.Println("         netsh int ipv6 set dynamicport tcp start=1025 num=64511")
	return nil
}
