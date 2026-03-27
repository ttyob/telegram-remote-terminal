package telegram

import "strings"

// sanitizeTerminalOutput removes common terminal control sequences so Telegram
// receives readable plain text.
func sanitizeTerminalOutput(s string) string {
	if s == "" {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); {
		c := s[i]

		if c == 0x1b {
			if i+1 >= len(s) {
				i++
				continue
			}
			next := s[i+1]

			// CSI: ESC [ ... final-byte(0x40-0x7E)
			if next == '[' {
				i += 2
				for i < len(s) {
					if s[i] >= 0x40 && s[i] <= 0x7e {
						i++
						break
					}
					i++
				}
				continue
			}

			// OSC: ESC ] ... BEL or ST(ESC \)
			if next == ']' {
				i += 2
				for i < len(s) {
					if s[i] == 0x07 {
						i++
						break
					}
					if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
				continue
			}

			// Two-byte escape sequence.
			i += 2
			continue
		}

		// Keep tabs/newlines; normalize standalone carriage return to newline.
		if c == '\r' {
			if i+1 < len(s) && s[i+1] == '\n' {
				b.WriteByte('\n')
				i += 2
				continue
			}
			b.WriteByte('\n')
			i++
			continue
		}

		if c == '\n' || c == '\t' || c >= 0x20 {
			b.WriteByte(c)
		}
		i++
	}

	return b.String()
}
