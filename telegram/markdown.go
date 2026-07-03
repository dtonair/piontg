package telegram

import (
	"strings"
	"unicode/utf8"
)

const telegramMarkdownParseMode = "MarkdownV2"

// telegramMarkdown formats a small, safe subset of CommonMark-like assistant
// output for Telegram MarkdownV2. Unsupported markdown is escaped and shown as
// plain text so Telegram does not reject the message.
func telegramMarkdown(text string) string {
	var b strings.Builder
	startOfLine := true

	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], "```") {
			end := strings.Index(text[i+3:], "```")
			if end >= 0 {
				contentStart := i + 3
				contentEnd := contentStart + end
				content := text[contentStart:contentEnd]
				if nl := strings.IndexByte(content, '\n'); nl >= 0 {
					content = content[nl+1:]
				}
				b.WriteString("```\n")
				b.WriteString(escapeMarkdownV2Code(content))
				b.WriteString("```")
				i = contentEnd + 3
				startOfLine = false
				continue
			}
		}

		if startOfLine {
			if headingLevel, headingStart := markdownHeading(text[i:]); headingLevel > 0 {
				lineEnd := strings.IndexByte(text[i+headingStart:], '\n')
				if lineEnd < 0 {
					b.WriteByte('*')
					b.WriteString(escapeMarkdownV2Text(strings.TrimSpace(text[i+headingStart:])))
					b.WriteByte('*')
					return b.String()
				}
				b.WriteByte('*')
				b.WriteString(escapeMarkdownV2Text(strings.TrimSpace(text[i+headingStart : i+headingStart+lineEnd])))
				b.WriteByte('*')
				b.WriteByte('\n')
				i += headingStart + lineEnd + 1
				startOfLine = true
				continue
			}
		}

		if strings.HasPrefix(text[i:], "**") {
			if end := strings.Index(text[i+2:], "**"); end >= 0 {
				contentStart := i + 2
				contentEnd := contentStart + end
				b.WriteByte('*')
				b.WriteString(escapeMarkdownV2Text(text[contentStart:contentEnd]))
				b.WriteByte('*')
				i = contentEnd + 2
				startOfLine = false
				continue
			}
		}

		if text[i] == '`' {
			if end := strings.IndexByte(text[i+1:], '`'); end >= 0 {
				contentStart := i + 1
				contentEnd := contentStart + end
				b.WriteByte('`')
				b.WriteString(escapeMarkdownV2Code(text[contentStart:contentEnd]))
				b.WriteByte('`')
				i = contentEnd + 1
				startOfLine = false
				continue
			}
		}

		r, size := utf8.DecodeRuneInString(text[i:])
		b.WriteString(escapeMarkdownV2Rune(r))
		startOfLine = r == '\n'
		i += size
	}
	return b.String()
}

func markdownHeading(s string) (level int, contentStart int) {
	for level < len(s) && level < 6 && s[level] == '#' {
		level++
	}
	if level == 0 || level >= len(s) || s[level] != ' ' {
		return 0, 0
	}
	return level, level + 1
}

func escapeMarkdownV2Text(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteString(escapeMarkdownV2Rune(r))
	}
	return b.String()
}

func escapeMarkdownV2Code(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '`', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func escapeMarkdownV2Rune(r rune) string {
	switch r {
	case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!', '\\':
		return "\\" + string(r)
	default:
		return string(r)
	}
}

func isTelegramParseError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "can't parse entities") || strings.Contains(msg, "can't find end of")
}
