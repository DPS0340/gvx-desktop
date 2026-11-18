package ui

import (
	"sync"
	"sync/atomic"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/i18n"
	"github.com/ironpark/ggui/ui"
)

// The languages the app is worded in. English is the source: every string
// in the code is English, and a catalog maps it to another language.
const (
	LangSystem  = "system"
	LangEnglish = "en"
	LangKorean  = "ko"
)

// localizers translates into each language, keyed by it. Each is fixed to
// its language, a ggui.Const, so translating with it reads no state and
// may happen on any goroutine. English, the source, needs no catalog.
var localizers = map[string]*i18n.Localizer{
	LangEnglish: i18n.New(ggui.Const(LangEnglish)),
	LangKorean:  i18n.New(ggui.Const(LangKorean), korean),
}

// current is the Localizer tr translates with. It is atomic because toasts
// are worded from the goroutines that finish background work.
var current atomic.Pointer[i18n.Localizer]

func init() { current.Store(localizers[LangEnglish]) }

// localizer returns the Localizer of lang, English's for a language the
// app has no words for.
func localizer(lang string) *i18n.Localizer {
	if l, ok := localizers[lang]; ok {
		return l
	}
	return localizers[LangEnglish]
}

// SetLanguage chooses the language tr answers in from then on. Build calls
// it before it builds the window, so the tree is worded in one language.
func SetLanguage(lang string) { current.Store(localizer(lang)) }

// tr returns the text of key in the current language, or key itself where
// the language has no word for it. With args, key is a fmt format: a
// translation reorders them with explicit indexes such as %[2]s, and in
// Korean picks the particle after an argument ("%s{을/를}"). Without, the
// text is returned as written. Pass args that are plain values: a reactive
// one would be read, which only the UI goroutine may do.
func tr(key string, args ...any) string { return current.Load().S(key, args...) }

// trn is tr for a count: one is the English for n == 1, other for any
// other n, and n is the first argument. A catalog words it under one.
func trn(n int, one, other string, args ...any) string {
	return current.Load().N(n, one, other, args...)
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

// systemLanguage is the first of the user's languages the app has words
// for, English when none is. It is read once, so changing an unrelated
// setting does not change the language under the user.
var systemLanguage = sync.OnceValue(func() string {
	return i18n.Match([]string{LangEnglish, LangKorean}, ggui.SystemLanguages())
})

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
