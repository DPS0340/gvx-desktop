package ui

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ironpark/ggui/ui"
)

// The languages the app is worded in. English is the source: every string
// in the code is English, and a catalog maps it to another language.
const (
	LangSystem  = "system"
	LangEnglish = "en"
	LangKorean  = "ko"
)

// catalogs holds the translations of each language but English, keyed by
// the English text.
var catalogs = map[string]map[string]string{
	LangKorean: korean,
}

// current is the catalog tr reads, nil for English. It is atomic because
// toasts are worded from the goroutines that finish background work.
var current atomic.Pointer[map[string]string]

// SetLanguage chooses the language tr answers in from then on. Build calls
// it before it builds the window, so the tree is worded in one language.
func SetLanguage(lang string) {
	if c, ok := catalogs[lang]; ok {
		current.Store(&c)
		return
	}
	current.Store(nil)
}

// tr returns the text of key in the current language, or key itself where
// the language has no word for it. With args, key is a fmt format: a
// translation reorders them with explicit indexes such as %[2]s.
func tr(key string, args ...any) string {
	if c := current.Load(); c != nil {
		if v, ok := (*c)[key]; ok {
			key = v
		}
	}
	if len(args) == 0 {
		return key
	}
	return fmt.Sprintf(key, args...)
}

// T is tr for the menus and the tray, which main builds.
func T(key string, args ...any) string { return tr(key, args...) }

// ResolveLanguage turns the language of the settings into one the app has
// words for: the one chosen, or the system's when none is.
func ResolveLanguage(setting string) string {
	switch setting {
	case LangEnglish, LangKorean:
		return setting
	}
	return systemLanguage()
}

var systemLanguage = sync.OnceValue(func() string {
	for _, tag := range preferredLanguages() {
		tag = strings.ToLower(tag)
		switch {
		case strings.HasPrefix(tag, "ko"):
			return LangKorean
		case strings.HasPrefix(tag, "en"):
			return LangEnglish
		}
	}
	return LangEnglish
})

// preferredLanguages lists the user's languages, most wanted first. macOS
// keeps them in the user defaults, and an app opened from the Finder has
// no LANG; elsewhere the locale variables say.
func preferredLanguages() []string {
	var tags []string
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("defaults", "read", "-g", "AppleLanguages").Output(); err == nil {
			for _, f := range strings.FieldsFunc(string(out), func(r rune) bool {
				return r == '(' || r == ')' || r == ',' || r == '"' || r == ' ' || r == '\n' || r == '\t'
			}) {
				tags = append(tags, f)
			}
		}
	}
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" && v != "C" && v != "POSIX" {
			tags = append(tags, v)
		}
	}
	return tags
}

// languageName is how the settings name a language: each in its own words,
// so a reader finds theirs whatever the app is set to.
func languageName(lang string) string {
	switch lang {
	case LangEnglish:
		return "English"
	case LangKorean:
		return "한국어"
	}
	return tr("System")
}

func themeName(theme string) string {
	switch theme {
	case "light":
		return tr("Light")
	case "dark":
		return tr("Dark")
	}
	return tr("System")
}

// controlStrings words the controls of ggui's ui package, which say
// "Close" or "No results" by themselves. A field left empty keeps the
// English.
func controlStrings(lang string) ui.Strings {
	if lang != LangKorean {
		return ui.Strings{}
	}
	return ui.Strings{
		Close:    "닫기",
		Previous: "이전",
		Next:     "다음",
		Loading:  "불러오는 중",

		DismissFormat:    "%s 닫기",
		CloseTabFormat:   "%s 닫기",
		AlertContinue:    "계속",
		AlertCancel:      "취소",
		DrawerSize:       "서랍 크기",
		DrawerSizeFormat: "%s 크기",

		SearchCommands:      "명령 검색",
		SearchCommandsHint:  "명령 검색…",
		Commands:            "명령",
		NoResults:           "결과 없음",
		HintNavigate:        "이동",
		HintSelect:          "선택",
		ComboboxPlaceholder: "선택…",
		ChooseOption:        "옵션 선택",
		ChooseOptions:       "옵션 선택",
		ShowOptions:         "옵션 보기",
		ClearValue:          "지우기",
		SearchOptions:       "옵션 검색",
		NoItemsFound:        "항목이 없습니다.",
		RemoveFormat:        "%s 제거",

		ThemeModeName:   "테마",
		ToggleGroupName: "옵션",
		ButtonGroupName: "동작",
		ContextMenuName: "컨텍스트 메뉴",
		MenubarName:     "메뉴 막대",
		NavigationName:  "탐색",
		SidebarName:     "사이드바",
		DarkMode:        "다크 모드",
		SliderValue:     "값",

		ThemeLight:        "라이트",
		ThemeDark:         "다크",
		ThemeSystem:       "시스템",
		ToggleSidebar:     "사이드바 전환",
		NumberDecrement:   "감소",
		NumberIncrement:   "증가",
		CodeContent:       "코드 내용",
		CodeScrollHelp:    "화살표 키나 Page Up, Page Down으로 스크롤합니다",
		CodeBlockName:     "코드 블록",
		CodeName:          "코드",
		CopyCode:          "코드 복사",
		CodeCopied:        "코드를 복사했습니다",
		ScrollToStart:     "처음으로 스크롤",
		ScrollToEnd:       "끝으로 스크롤",
		SortFormat:        "%s 정렬",
		SortAscending:     "오름차순",
		SortDescending:    "내림차순",
		NoTableResults:    "결과가 없습니다.",
		FilterRows:        "행 필터",
		FilterRowsHint:    "행 필터...",
		Columns:           "열",
		ShowColumnFormat:  "%s 표시",
		SelectRowFormat:   "%v 행 선택",
		SelectPage:        "페이지 선택",
		RowsPerPage:       "페이지당 행 수",
		PageFormat:        "%d 페이지",
		PageOfFormat:      "%[2]d 페이지 중 %[1]d",
		PaginationName:    "페이지 이동",
		MorePages:         "더 많은 페이지",
		Submit:            "제출",
		Skip:              "건너뛰기",
		AvatarMoreFormat:  "%d개 더",
		BreadcrumbMore:    "더 보기",
		BreadcrumbName:    "이동 경로",
		AccordionName:     "아코디언",
		ResizablePanels:   "패널 크기 조절",
		SliderThumbFormat: "%s %d",
	}
}
