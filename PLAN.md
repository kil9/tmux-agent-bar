# PLAN.md

tmux-agent-bar 개발 마스터 문서. 컨텍스트 리셋 후에도 이 파일 하나로 현재 상태와 다음 할 일을 파악한다.

## 목표

tmux 윈도우 이름 앞에 이모지를 자동으로 삽입하여, 여러 pane에서 실행 중인 Claude Code 에이전트의 상태(승인 대기 / 완료)를 한눈에 파악한다.

## 비목표

- 별도의 tmux 상태 바(status bar) 신규 추가
- Claude Code 이외의 AI 에이전트 지원 (초기 범위)
- GUI 인터페이스

## 제약

- tmux 세션 내에서만 동작 (tmux 없이 실행 시 무동작 또는 종료)
- 기존 tmux 설정(`~/.tmux.conf`)에 최소한의 변경만 요구해야 함
- 이모지를 윈도우 이름 앞에만 붙이고, 원래 이름을 변경하지 않음

---

## 확정된 기술 결정

| 항목 | 결정 | 이유 |
|------|------|------|
| 구현 언어 | **Go** | 단일 바이너리, 빠른 실행 |
| 상태 감지 | **Claude Code hooks** | 정확한 이벤트 기반 감지 |
| 윈도우 이름 업데이트 | **방식 B: `window-status-format`** | 원래 이름 보존, automatic-rename과 무충돌 |

### 동작 흐름

```
Claude Code 이벤트 발생
    → Claude Code hook 실행
    → tmux-agent-bar hook <thinking|waiting|done|error> 호출
    → $TMUX_PANE으로 현재 pane 식별
    → 상태 파일 기록 (/tmp/tmux-agent-bar/<session>_<window>_<pane>)

tmux status-interval (1초마다)
    → window-status-format 평가
    → tmux-agent-bar status <window_index> 호출
    → 해당 윈도우 pane들의 상태 파일 읽어 이모지 반환
    → 우선순위: 🚨 > 💬 > 🧠 > ✅ > ""
    → 윈도우 이름 (#I) 바로 뒤에 이모지 삽입
```

### tmux.conf 설정 (예정)

```
set -g status-interval 1
set -g window-status-format "#(tmux-agent-bar status #{window_index})#I #W"
set -g window-status-current-format "#(tmux-agent-bar status #{window_index})#I #W"
```

### Claude Code hooks 설정

`~/.claude/settings.json` (또는 `tmux-agent-bar install`로 자동 추가):
```json
{
  "hooks": {
    "PreToolUse":  [{ "matcher": "", "hooks": [{ "type": "command", "command": "tmux-agent-bar hook thinking" }] }],
    "Stop":        [{ "matcher": "", "hooks": [{ "type": "command", "command": "tmux-agent-bar hook done" }] }],
    "Notification":[{ "matcher": "", "hooks": [{ "type": "command", "command": "tmux-agent-bar hook waiting" }] }]
  }
}
```

### 상태 파일 규약

- 경로: `/tmp/tmux-agent-bar/<session>_<window>_<pane>`
- 값: `thinking` / `waiting` / `done` / `error` / (파일 없음 = idle)

### 상태 우선순위 (윈도우 단위 집계)

`🚨` error > `💬` waiting > `🧠` thinking > `✅` done > `` idle

---

## 현재 상태 (2026-04-07)

- [x] 프로젝트 초기화 (git, README.md)
- [x] README.md, AGENTS.md, PLAN.md 문서화
- [x] 기술 결정 확정 (Go / hooks / window-status-format)
- [x] Go 프로젝트 초기화 및 핵심 기능 구현 (완료 2026-04-07)
- [x] 이모지 세트 확정: 🧠 thinking / 💬 waiting / ✅ done / 🚨 error
- [x] 실전 테스트 및 tmux.conf 포맷 수정 (기존 스타일 보존, #I 뒤에 이모지 삽입)
- [x] PreToolUse 훅 누락 버그 수정 (~/.claude/settings.json 직접 추가)

---

## Phase 2: 핵심 기능 구현

### 구현할 서브커맨드

| 커맨드 | 역할 |
|--------|------|
| `tmux-agent-bar status <window_index>` | 해당 윈도우의 이모지 반환 (tmux format에서 호출) |
| `tmux-agent-bar hook <status>` | Claude Code hook에서 호출, 상태 파일 기록 |

### 구현 항목

- [x] Go 프로젝트 초기화 (`go mod init`)
- [x] `hook` 서브커맨드: 현재 tmux pane 정보 읽어 상태 파일 기록
- [x] `status` 서브커맨드: 윈도우 내 pane 상태 집계 후 이모지 반환
- [x] 상태 파일 읽기/쓰기 모듈
- [x] tmux pane 목록 조회 (`tmux list-panes`) 모듈

### 완료 기준

- `tmux-agent-bar status 2` 실행 시 올바른 이모지 출력
- Claude Code hook 트리거 시 상태 파일이 정상 기록됨

---

## Phase 3: tmux 연동 및 자동화

- [x] `~/.tmux.conf` 설정 가이드 완성 (README.md)
- [x] Claude Code `~/.claude/settings.json` 설정 가이드 완성 (README.md)
- [x] (선택) 설정 자동화 스크립트 (`tmux-agent-bar install`)

---

## Phase 4: 테스트 및 안정화

- [x] 다중 윈도우 / 다중 pane 시나리오 테스트 (집계 로직이 pane 목록 기반으로 동작)
- [x] 엣지 케이스 처리
  - Claude Code가 없는 pane → TMUX_PANE 없으면 무동작
  - tmux 세션 밖에서 실행 → status는 빈 문자열 반환
  - 오래된 상태 파일 정리 → hook 실행 시 cleanStaleFiles() 호출
- [x] 성능 검증: ~5.6ms/call (status-interval 1초 기준 충분)

---

## Phase 5: 리팩토링 (완료 2026-04-07)

### 버그 / 단순 수정

- [x] usage 메시지 오기재 수정 (main.go:23) — `"hook <waiting|done>"` → `"hook <thinking|waiting|done|error>"`
- [x] `fmt.Print("")` 제거 — 빈 문자열 출력은 no-op, `return`으로 대체
- [x] 미사용 타입 `hookEntry`, `hookCmd` 삭제 — `installClaudeSettings`가 `map[string]any` 사용 중

### 구조적 중복 제거

- [x] `stateKey(session, window, pane) string` 헬퍼 추출 — `session+"_"+windowIndex+"_"+pane` 패턴이 여러 곳에 반복됨
- [x] `emojiForStates(states []string) string` 분리 — `aggregateWindowEmoji`(main.go)와 `aggregateWindowEmojiFromDir`(main_test.go)의 switch 문이 동일. 상태 추가 시 두 곳을 같이 수정해야 하는 문제 해결
- [x] `cleanStaleFiles(dir, session, window, alivePanes)` 로 시그니처 변경 — 프로덕션은 `stateDir` 전달, 테스트는 tmpDir 전달. `cleanStaleFilesFromDir` 테스트 헬퍼 제거
- [x] `setStateDirForTest` 실제 동작으로 개선 (`stateDir` var로 변경), `TestWriteAndReadState`를 실제 `writeState`/`readState` 테스트로 개선

### install 커맨드 idempotent 개선

- [x] 이벤트별로 누락된 훅만 추가하는 방식으로 변경 — 새 이벤트 추가 시 기존 장비에서도 누락 없이 설치됨

---

## 알려진 한계 (Claude Code hooks 제약)

- **도구 없는 응답에서 🧠 미표시**: `PreToolUse`가 없으면 thinking 상태가 기록 안 됨. Claude가 텍스트만 응답할 때는 🧠가 안 보임
- **✅ 자동 클리어 없음**: 새 대화 시작 시 이전 `done` 상태가 남아있다가 첫 도구 사용 시점에 🧠로 전환됨. Claude Code에 "대화 시작" 훅이 없어서 해결 어려움
