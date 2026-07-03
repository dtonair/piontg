package telegram

import "testing"

func TestTelegramMarkdownFormatsCommonAssistantMarkdown(t *testing.T) {
	input := "## Title!\nRepo **piontg** uses `pi --mode rpc`.\n- done"
	want := "*Title\\!*\nRepo *piontg* uses `pi --mode rpc`\\.\n\\- done"
	if got := telegramMarkdown(input); got != want {
		t.Fatalf("telegramMarkdown() = %q, want %q", got, want)
	}
}

func TestTelegramMarkdownEscapesUnclosedFormatting(t *testing.T) {
	input := "Partial **bold and `code"
	want := "Partial \\*\\*bold and \\`code"
	if got := telegramMarkdown(input); got != want {
		t.Fatalf("telegramMarkdown() = %q, want %q", got, want)
	}
}

func TestTelegramMarkdownPreservesUnicodeText(t *testing.T) {
	input := "Hey đ Muốn mình xem phần nào của repo `piontg` không?"
	want := "Hey đ Muốn mình xem phần nào của repo `piontg` không?"
	if got := telegramMarkdown(input); got != want {
		t.Fatalf("telegramMarkdown() = %q, want %q", got, want)
	}
}

func TestTelegramMarkdownPreservesCodeBlocks(t *testing.T) {
	input := "```go\nfmt.Println(`x`)\n```"
	want := "```\nfmt.Println(\\`x\\`)\n```"
	if got := telegramMarkdown(input); got != want {
		t.Fatalf("telegramMarkdown() = %q, want %q", got, want)
	}
}
