# PLAN.md

`tmux-agent-bar` 의 작업 계획.

## 현재 상태 (Snapshot)

- 표시 상태: `🚨` (error) > `💬` (waiting) > `⏸` (planning) > `🤖` (thinking) > `⏳` (bg_waiting) > `✅` (done) > idle
- Claude Code hook 6종(`UserPromptSubmit`, `PreToolUse`, `Notification`, `Stop`, `SubagentStop`, `SessionEnd`)을 통해 상태 파일을 `/tmp/tmux-agent-bar/<key>` 에 기록한다.
- `status-interval`(기본 30초) 주기의 `runStatus` 가 각 윈도우의 pane 상태를 집계해 윈도우 이름 앞에 이모지를 삽입한다. ⏳ 판정과 orphan GC(5분 throttle)도 이 렌더 경로에서 수행한다.

## 다음 할 일

### 1. Claude 가 백그라운드 작업을 띄워두고 대기 중일 때 별도 상태(⏳) 표시 ✅ 완료 (2026-05-22)

**배경**: 현재 Claude Code 가 Monitor / `Bash run_in_background` / shell 등을 띄워두고 외부 이벤트를 기다리는 동안에도 `Stop` hook 이 호출되어 `✅`(완료) 로 표시된다. 실제로는 "끝나지 않고 깨어날 신호를 기다리는 중" 이라 시각적으로 완료처럼 보이는 것이 어색하다. 백그라운드 대기 상태를 별도 시각 신호로 분리한다.

**목표**:
- 새 상태(가칭 `bg_waiting`, 최종 명명은 구현 단계에서 확정)를 추가하고 `⏳` 이모지로 표시한다.
- 우선순위: `🚨 > 💬 > ⏸ > 🤖 > ⏳ > ✅ > idle` (🤖과 ✅ 사이).
- 감지: `Stop` hook 시점에 `TMUX_PANE` 의 `pane_pid` 하위 자식 프로세스 트리를 검사해 살아있는 자식이 있으면 `done` 대신 `bg_waiting` 으로 기록한다.
- 보강: `runStatus` 렌더링 단계에서 `bg_waiting` 상태인 pane 의 자식이 모두 사라졌으면 자동으로 idle 로 전환한다(상태 파일 제거 또는 만료 처리).

**비목표**:
- Claude Code hook 페이로드 형식 변경/제안.
- 자식 프로세스가 "정말로 Claude 의 백그라운드 잡인지" 정밀 식별(예: 명령어 패턴 매칭). 1차 구현은 "살아있는 자식 존재" 단순 판정.
- `⏳` 상태에 경과 시간 표시(필요 시 별도 태스크).

**제약**:
- 감지는 `/proc/<pid>/task/<tid>/children` 또는 `/proc/<pid>/stat`의 ppid 트리 순회로 구현하며, hook 처리 시간 예산(현 `timeout budget 900ms` 정책)을 넘기지 않는다.
- 기존 우선순위 함수 `emojiForStates` 와 hook switch-case 의 유효 상태 목록을 호환 유지하며 확장한다.
- 새 상태에 대한 우선순위 / 매핑 단위 테스트와 자식 검사 함수 테스트(자식 있음/없음/PID 미존재)를 함께 추가한다.

**검증**:
- 단위 테스트: `emojiForStates` 가 `bg_waiting` 입력에 `⏳` 를 반환하고 우선순위가 `🤖` 보다 낮고 `✅` 보다 높음을 확인한다.
- 단위 테스트: 자식 프로세스 검사 함수가 fixture 디렉터리 기반으로 `true/false` 를 올바르게 반환한다.
- 수동 시나리오: tmux pane 에서 Claude 가 `Bash run_in_background` 로 장기 실행 명령을 띄운 뒤 응답을 끝낸 직후 → 윈도우 라벨이 `⏳` 로 표시된다. 백그라운드 잡 종료 후 다음 status tick 에 `⏳` 가 사라진다.

### 2. 사라진 window/세션의 잔여 상태 파일 GC ✅ 완료 (2026-06-27)

**배경**: `cleanStaleFiles` 는 "현재 렌더링 중인 window 안의 닫힌 pane" 만 정리한다. window 나 세션이
통째로 사라지면 `runStatus` 가 그 window 를 다시 순회하지 않아 상태 파일이 /tmp 가 비워질 때까지
누적된다. uptime 이 긴 머신에서 며칠 전 파일이 그대로 남는 문제가 관측됨.

**구현**: `cleanOrphanState` 가 status tick 중(최대 `orphanGCInterval`=5분 간격, `.gc` 마커로 throttle)
`tmux list-windows -a -F "#S_#I"` 로 살아있는 window 키 집합을 만들고, 어떤 live window 키로도
시작하지 않는 파일을 삭제한다. 죽은 window 와 죽은 세션을 모두 포괄한다.

### 3. `⏳`(bg_waiting) 경과 시간 표시 ✅ 완료 (2026-06-27)

**배경**: 태스크 1 에서 비목표로 남겨둔 항목. 백그라운드 대기가 길어질 때 얼마나 기다렸는지 보이지 않음.

**구현**: 상태 파일(렌더 시점 판정 통합 후로는 Stop 이 기록한 `done`)은 대기 중 재기록되지 않으므로
그 mtime 을 대기 시작 시각으로 재사용한다. `runStatus` 에서 🤖 와 동일하게 `formatElapsed`(분 단위)
로 표시한다. 🤖/⏳ 공통 접미사 렌더링은 `elapsedSuffix` 로 추출.

---

아래 태스크 4-9 는 2026-07-02 점검(2차)에서 발견된 항목의 일괄 처리 계획이다. **순서대로 진행**하고,
태스크마다 별도 커밋으로 분리하며, push 는 `origin`(oss.navercorp)과 `gh`(github.com) 양쪽에 한다.
gh 는 ssh config 로 `ssh.github.com:443` 우회이므로 push 전 `nc -z -w4 ssh.github.com 443` 으로
도달성을 확인한다. 코드 변경 태스크는 `gofmt` + `go vet` + `go test ./...` 통과 후
`make install` + GOBIN 사본(`cp tmux-agent-bar $(go env GOPATH)/bin/`) 갱신까지가 완료 조건이다.

### 4. macOS(/proc 부재) 회귀 수정 — claude-right 가 meta 를 지우고 ctx% 를 영구 숨김 ✅ 완료 (2026-07-02)

**배경**: 2026-07-02 에 넣은 claude-right stale meta 가드는 `findClaudeDescendants`(=
`/proc/<pid>/task/<pid>/children` 읽기, Linux 전용)가 빈 결과면 "살아있는 claude 없음" 으로 보고
`.meta` 를 삭제하고 inactive 를 출력한다. macOS 는 /proc 이 없어 **항상** 빈 결과 → ctx%+model
세그먼트가 절대 표시되지 않고 meta 가 hook 이 다시 쓸 때마다 지워진다. bootstrap 이 macOS(launchd)
를 지원하므로 실제 영향권.

**구현 방향**: `procTreeAvailable() bool` 헬퍼를 추가해 자기 자신의 children 파일
(`procRoot/<getpid>/task/<getpid>/children`)이 읽히는지로 판정하고, 불가하면 liveness 가드를
건너뛰어 meta 를 신뢰한다(삭제 금지). `paneHasBackgroundJobs` 는 macOS 에서 자연히 false(⏳ 미표시,
✅ 유지)라 손대지 않는다. 판정 기준을 procRoot 존재 여부가 아니라 children 파일 가독성으로 하는
이유: 일부 시스템은 /proc 은 있어도 children 파일(CONFIG_PROC_CHILDREN)이 없을 수 있다.

**검증**: liveness 판정을 헬퍼 함수로 추출해 단위 테스트(procRoot=fixture 에서 self 항목이 없으면
가드 skip 을 확인). Linux 실기기에서 기존 동작(살아있는 claude → 표시, 죽은 pane → meta 삭제) 회귀
없음 확인.

### 5. install 안정화 — 워치독 제외 + settings.json 원자적 쓰기 ✅ 완료 (2026-07-02)

**배경**: main() 의 900ms 워치독 고루틴은 서브커맨드와 무관하게 `os.Exit(124)` 를 호출한다.
`install` 이 900ms 를 넘기면 `os.WriteFile`(truncate 후 쓰기) 도중 죽어 `~/.claude/settings.json`
이 깨질 수 있다. 확률은 낮지만 결과가 치명적(모든 hook 소실).

**구현 방향**: (1) `install` 서브커맨드는 워치독을 기동하지 않는다(status/claude-right/hook 만 대상).
(2) `installClaudeSettings` 의 settings.json 쓰기를 상태 파일과 동일하게 temp+rename
(`writeFileAtomic` 재사용, 단 대상 디렉토리가 stateDir 가 아니므로 경로 주의)으로 바꾼다.
tmux.conf 는 O_APPEND 라 부분 쓰기 리스크가 낮아 유지.

**검증**: `tmux-agent-bar install` 재실행(멱등 — "already configured" 경로) 후 settings.json 이
유효 JSON 인지 `python3 -c "import json; json.load(open(...))"` 로 확인. 단위 테스트는 어려우므로
수동 스모크로 갈음.

### 6. 세션명 `_` 엣지 제거 — key 역파싱(SplitN) 폐지 ✅ 완료 (2026-07-02)

**배경**: runHook 은 `tmuxPaneKey` 가 만든 `<session>_<window>_<pane>` 문자열을
`strings.SplitN(key, "_", 3)` 으로 역파싱해 cleanStaleFiles 인자로 쓴다(2곳: waiting 분기,
말미 정리). 세션명에 `_` 가 있으면 파싱이 어긋나 그 세션의 pane 정리가 무력화된다(잘못 지우진 않음
— tmuxListPanes 가 실패해 skip. orphan GC 가 결국 회수하므로 실해는 없지만 코드 냄새).

**구현 방향**: `tmuxPaneKey` 를 `tmuxPaneParts(paneID) (session, window, pane string, err error)`
로 바꾸거나 병행 추가한다. tmux 조회 포맷을 `#S_#I_#P` 에서 탭 구분(`#S\t#I\t#P`)으로 바꿔 세 값을
직접 받고, key 는 `stateKey(session, window, pane)` 로 조립한다. SplitN 역파싱 2곳을 제거한다.
상태 파일명 자체의 이론적 충돌(세션 "a_1" 창 "0" vs 세션 "a" 창 "1_0")은 비목표 — orphan GC 의
prefix 매칭은 live 키 기준이라 안전하고, 실사용 세션명에서 충돌 조합이 나올 가능성이 낮다.

**검증**: 단위 테스트(탭 구분 출력 파싱), 세션명에 `_` 가 들어간 tmux 세션을 만들어
(`tmux new-session -d -s "under_score"`) hook 스모크 후 상태 파일 생성/정리 확인, 세션 삭제.

### 7. `[ ] TODO` 문서·잔재 소소한 정리 (한 커밋으로 묶음)

- README "여러 pane 상태 집계 규칙" 예시가 초 단위(`🤖(30s)`, `🤖(5s)`)로 남아 있음 → 분 단위
  표시 규칙에 맞게 수정(예: `🤖(30m)`, `🤖(5m)` 또는 "1분 미만은 숨김" 각주).
- `procStartTime` 만 `/proc` 하드코딩 → 다른 proc 헬퍼처럼 `procRoot` 를 쓰도록 통일
  (`/proc/stat` 의 btime 읽기 포함). 기존 TestProcStartTime 이 실 /proc 을 쓰므로 procRoot 기본값
  하에서 계속 통과하는지 확인.
- `writeFileAtomic` 이 크래시로 남긴 `.tmp-*` 는 dotfile 이라 orphan GC 가 건너뜀 → removeOrphanFiles
  에서 `.tmp-` prefix 이고 mtime 1시간 이상이면 삭제하는 예외 추가 + 단위 테스트.
- AGENTS.md 의 "이 저장소 전용 정보" 에 세션명 `_` 처리(태스크 6 반영 후 상태)와 macOS 폴백 동작
  (태스크 4)을 한 줄씩 기록.

### 8. `[ ] TODO` ctx% 분모 머신 로컬 설정 (kil9conf 연계)

**배경**: 이 머신(사내 워크스테이션)은 1M 컨텍스트 세션 위주인데 기본 분모 200k 라 ctx% 가 과대
표시된다(실측: 이 세션이 100% 로 표시). 전 머신 공통(rc/tmux.conf)으로 1M 을 박으면 200k 플랜
머신에서 과소 표시되므로 **머신 로컬 오버라이드 통로**를 만든다.

**구현 방향** (kil9conf repo, `~/kil9conf` 에서 작업):
1. `rc/tmux.conf` 말미에 `source-file -q ~/.tmux.local.conf` 추가(파일 없으면 조용히 무시 — `-q`).
2. 이 머신에만 `~/.tmux.local.conf` 를 만들어
   `set-environment -g TMUX_AGENT_BAR_CTX_LIMIT 1000000` 기록(이 파일은 kil9conf 미추적).
3. 즉시 반영: `tmux set-environment -g TMUX_AGENT_BAR_CTX_LIMIT 1000000` +
   `tmux source-file ~/.tmux.conf`. status-right 의 ctx% 가 낮아졌는지 눈으로 확인.
4. kil9conf 커밋은 kil9conf 저장소 컨벤션(AGENTS.md/README 갱신 검토 포함)으로 별도 수행.
   머신 로컬 파일의 존재는 Claude auto-memory 에 기록(기기별 로컬 규칙).

주의: tmux `#()` 커맨드가 `set-environment -g` 값을 상속하는지 이 머신에서 실측 확인할 것.
안 되면 tmux 서버 기동 전 셸 export(예: zshrc 의 머신 로컬 분기)로 대체한다.

### 9. `[ ] TODO` 잔존 agent worktree 정리

`git worktree list` 에 남은 `.claude/worktrees/agent-acac9dfa`(브랜치 `worktree-agent-acac9dfa`,
tip ffba347)는 main 과 계보가 끊긴 초기 구축 이력(Phase 1-4)으로, 내용은 현재 main 에 모두
대체·반영돼 있다. `git worktree remove .claude/worktrees/agent-acac9dfa` 후
`git branch -D worktree-agent-acac9dfa` 로 삭제한다(-D 필요: 미병합 계보). 삭제 전 브랜치
tip 해시(ffba347)를 이 파일 진행 로그에 남겨 reflog 복구 여지를 확보한다.

### 10. `[ ] TODO` 🤖(thinking) 이모지를 모델별 이모지로 전환 (ccstatusline 매핑 정합)

**배경**: ccstatusline 은 모델별로 맨 앞 이모지를 전환한다(kil9conf 32864f7:
✨ Fable · 🌀 Opus · 🤖 Sonnet · 🖥️ Haiku, 매칭 실패 시 기존값 유지). tmux-agent-bar 의
진행중 표시는 항상 🤖 라 창 이름만으로는 어떤 모델이 돌고 있는지 알 수 없다. pane `.meta` 에
model ID 가 이미 기록되므로(hook 이 transcript 에서 추출) 추가 데이터 없이 구현 가능 — 조사 완료.

**구현 방향**:
- `modelEmoji(model string) string` 헬퍼 추가: `shortModelName` 과 동일한 tier substring 매칭으로
  fable/mythos→✨, opus→🌀, sonnet→🤖, haiku→🖥️, 그 외·모델 불명→🤖(현행 유지).
  ccstatusline 은 display_name("Opus") 매칭, 여기는 model ID("claude-opus-4-8") — tier 매칭으로 흡수.
- 적용 지점은 `runStatus` 의 표시 계층: 집계 결과가 🤖 일 때만 교체. `emojiForStates` 와 상태
  우선순위 로직은 손대지 않는다(상태 의미 불변, 표기만 전환).
- 다중 pane: 경과시간과 같은 pane 을 가리키도록 `thinkingStartTime` 의 선택 규칙(last-active
  thinking pane, 없으면 earliest)과 동일한 pane 의 meta 를 읽는다. 그 pane 의 meta 부재 시 🤖 폴백.

**제약/리스크**:
- 🖥️(U+1F5A5+VS16) 는 조합 이모지라 터미널/tmux 폭 계산이 어긋날 수 있다 — 실제 status bar 에서
  폭 깨짐을 눈으로 확인하고, 문제면 haiku 대체 이모지를 ccstatusline 쪽과 함께 조정한다.
- README 상태 표(처리 중 `🤖(5m)` 예시)와 AGENTS.md 의 상태 우선순위 서술에 모델별 전환을 반영한다.

**검증**: `modelEmoji` 단위 테스트(각 tier·불명·빈 문자열), 실기기에서 fable 세션 창이 `✨(Nm)` 로
뜨는지, 💬/⏳/✅ 등 다른 상태 이모지는 불변인지 확인.

### 후속 수동 작업 (코드 밖, 세션에서 자동화 불가)

- 다른 머신들(개인 머신, macOS 등)에서 `bootstrap/install-tmux-agent-bar.sh --upgrade` 실행해
  aab7fa5 이후 버전 반영. 특히 개인 머신은 bg_waiting 렌더 시점 통합의 영향을 받는다.
- macOS 머신은 태스크 4 반영 전까지 claude-right ctx% 가 안 뜨는 회귀 상태이므로 태스크 4 를
  먼저 끝내고 업그레이드할 것.

## 디자인 결정

### 디자인 결정: orphan GC 를 window 키 prefix 매칭으로 처리
- **날짜**: 2026-06-27
- **결정**: live window 키(`<session>_<window>`)를 모아 파일명이 `<키>_` 로 시작하는지로 생존 판정한다.
  세션 단위가 아니라 window 단위로 보므로 "세션은 살아있지만 window 만 닫힌" 잔여 파일도 잡는다.
  trailing `_` 를 붙여 `main_1` 이 `main_10_*` 를 오삭제하지 않게 한다.
- **이유**: 안전성(live 파일은 절대 삭제 안 함)을 우선. live window 키 prefix 에 매칭되지 않을 때만
  삭제하므로, 세션명/창번호에 `_` 가 섞인 경계에서도 live 파일을 지우는 일은 없다.
- **대안**: (1) 세션 단위 GC → 닫힌 window 잔여물을 못 잡음. (2) 파일명 split 파싱 → 세션명에 `_` 가
  있으면 깨짐.

### 디자인 결정: `bg_waiting` 감지를 "pane shell → claude → 자식 존재" 1단계 휴리스틱으로 한정 (2026-07-02 폐기)
- **날짜**: 2026-05-22
- **결정**: `paneHasBackgroundJobs` 는 pane shell PID 의 직접 자식 중 `comm` 에 "claude" 가 포함된 프로세스를 찾고, 해당 claude 의 자식이 1개 이상 있을 때만 true 를 반환한다. 후손 트리 전체 탐색이나 명령어 패턴 매칭은 하지 않는다.
- **이유**: 1차 구현 목표는 "Bash run_in_background / Monitor 가 살아있는 흔한 경우" 를 잡는 것이다. 직접 자식 두 단계만 보면 /proc 접근 비용이 매우 작고(`/proc/<pid>/task/<pid>/children` 두 번), 잘못 매칭될 가능성도 낮다.
- **대안**: (1) 후손 트리 BFS 전수 조사 → 비용이 더 들고 오탐 증가. (2) `cmdline` 패턴 매칭 → 너무 fragile.

### 디자인 결정: `bg_waiting` 판정을 Stop hook 시점에서 status 렌더 시점으로 이동
- **날짜**: 2026-07-02
- **결정**: Stop hook 은 `done` 만 기록하고, `runStatus` 렌더 경로에서 `done` 인 pane 에 살아있는 claude 백그라운드 잡이 있으면 ⏳ 로 표시한다(상태 파일은 `done` 유지 — 잡 종료 후 ✅ 복귀). claude 탐색은 직접 자식이 아니라 프로세스 트리 최대 4단계 BFS(`findClaudeDescendants`)로 하고, 잡 카운트는 **셸 comm(bash/sh/zsh/dash) 자식만** 대상으로 하며(도커/파이썬 MCP 서버 등 상주 인프라 자식 오탐 방지 — 실기기에서 claude 가 docker·python·statusline 래퍼를 상시 자식으로 가짐을 확인), 자기 자신·조상 PID 를 제외한다.
- **이유**: (1) npm 설치 claude 는 `shell → node(shim) → claude` 체인이라 직접 자식 매칭이 항상 실패해 기존 감지가 완전히 죽어 있었다(실기기 확인). (2) Stop hook 시점에는 hook 프로세스 자신과 병렬 실행되는 다른 Stop hook 들이 claude 의 자식이라 "자식 존재" 신호가 오염된다. (3) tmux 는 `#()` 를 status-interval 마다만 재실행하므로 렌더 시점 판정으로 옮겨도 사용자에게 보이는 지연은 동일하다.
- **대안**: (1) Stop hook 에서 자기 자신만 제외 → 병렬 sibling hook 오탐 잔존. (2) 자식 프로세스 나이 기반 필터 → "잡 시작 직후 Stop" 인 주 사용 사례를 놓침.

### 디자인 결정: 자동 idle 전환을 `runStatus` 렌더 경로에서 처리
- **날짜**: 2026-05-22
- **결정**: `bg_waiting` 상태 해제 책임은 1초 주기 `runStatus` (→ `aggregateWindowEmoji` → `resolvePaneStateOrClear`) 에 둔다. 자식이 사라졌으면 상태 파일을 제거해 idle 로 돌린다.
- **이유**: Claude Code 가 백그라운드 잡 종료를 별도 hook 으로 알려주지 않는다. 별도 데몬을 띄우지 않고 기존 status tick 을 재활용하면 의존성과 코드량이 최소다.
- **대안**: (1) 별도 watch 데몬 띄움 → 운영 복잡도 증가. (2) Notification hook 의존 → 잡 종료가 항상 Notification 을 부르지는 않음.

## 변경 이력

- 2026-05-22: 태스크 1(`bg_waiting` 상태 도입) 신규 등록.
- 2026-05-22: 태스크 1 완료. 디자인 결정 2건 기록.
- 2026-06-27: 태스크 2(orphan window/세션 GC), 태스크 3(`⏳` 경과 시간) 완료. 디자인 결정 1건 기록.
- 2026-07-02: 점검에서 발견된 이슈 일괄 수정 — ⏳ 판정 렌더 시점 이동(npm shim 체인 대응 + 자기/조상 제외), `SessionEnd` hook 으로 pane 파일 즉시 정리, claude-right 에 stale meta 가드(살아있는 claude 없으면 미표시+정리), `TMUX_AGENT_BAR_CTX_LIMIT` 환경변수(1M 컨텍스트 대응), status 타임아웃 폴백 ⏳→⌛ 충돌 해소, install 템플릿 status-right 색 정합(colour66), 상태 파일 원자적 쓰기, `shortModelName` fable/mythos 추가, 죽은 코드(`recentSubagentStop`) 제거.
- 2026-07-02: github/main(6/27-28 개인 머신 작업) 병합 — ⏳ 판정은 렌더 시점 방식으로 통일하고, 셸 comm 허용목록에 `looksLikeMCPServer` cmdline 가드를 결합. GC 는 orphan GC(window 키 prefix, 5분 throttle)를 채택하고 7일 mtime GC 는 폐기. ⏳ 경과시간 표시는 resolved 상태 기준으로 각색해 채택.
- 2026-07-02: 2차 점검 항목으로 태스크 4-9 등록. 방침(기존 PLAN.md 통합, ctx% 머신 로컬 오버라이드, 세션명 `_` 코드 수정, worktree 삭제)은 사용자 무응답으로 권장안 기준 — 시작 전 변경 가능.
