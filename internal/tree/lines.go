package tree

import (
	"bytes"

	"github.com/chenyiwen/p37/internal/codec"
)

// 行切分规则（§3.5）：
//
//   - 按 '\n' 切分；行的内容**不含** '\n'，但保留行尾的 '\r'
//   - 切分与拼接严格互逆：SplitLines(JoinLines(x)) == x，JoinLines(SplitLines(d)) == d
//
// 因此空白差异不会被悄悄抹除，rebase 的锚点匹配也就不会出现「看起来一样但
// 字节不同」的意外。

// SplitLines 按 '\n' 切分字节串，保留每行尾部的 '\r'。
//
// 空输入返回一个空行（长度为 1 的切片），以保证与 JoinLines 互逆。
func SplitLines(data []byte) [][]byte {
	if len(data) == 0 {
		return [][]byte{{}}
	}
	n := bytes.Count(data, []byte{'\n'})
	lines := make([][]byte, 0, n+1)
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	lines = append(lines, data[start:])
	return lines
}

// JoinLines 是 SplitLines 的逆运算。
func JoinLines(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	size := len(lines) - 1
	for _, l := range lines {
		size += len(l)
	}
	out := make([]byte, 0, size)
	for i, l := range lines {
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, l...)
	}
	return out
}

// EOL 取值与 codec 一致（0=lf, 1=crlf, 2=mixed）。
const (
	EOLlf    uint8 = 0
	EOLcrlf  uint8 = 1
	EOLmixed uint8 = 2
)

// DetectEOL 判定换行风格（§3.5）。
func DetectEOL(data []byte) uint8 {
	crlf, lfOnly := 0, 0
	for i := 0; i < len(data); i++ {
		if data[i] != '\n' {
			continue
		}
		if i > 0 && data[i-1] == '\r' {
			crlf++
		} else {
			lfOnly++
		}
	}
	switch {
	case crlf == 0:
		return EOLlf
	case lfOnly == 0:
		return EOLcrlf
	default:
		return EOLmixed
	}
}

// NormalizeEOL 去掉所有行尾的 '\r'，用于判定「仅换行差异」（§3.5）。
//
// 注意：它只用于**判定**，不用于存储，也不用于生成 op 内容。
func NormalizeEOL(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			continue
		}
		out = append(out, data[i])
	}
	return out
}

// IsEOLOnlyChange 报告两个内容是否仅在换行风格上不同（§3.5）。
//
// 若为真，采集应产出 kind=attr 的 op，而不产生任何内容 op。
func IsEOLOnlyChange(old, new []byte) bool {
	if bytes.Equal(old, new) {
		return false
	}
	return bytes.Equal(NormalizeEOL(old), NormalizeEOL(new))
}

// IsBinary 按 §3.5 判定二进制：前 probeBytes 字节中含 '\x00'。
func IsBinary(data []byte, probeBytes int) bool {
	n := min(len(data), probeBytes)
	return bytes.IndexByte(data[:n], 0) >= 0
}

// DefaultBinaryProbeBytes 是 §3.5 的默认探测长度。
const DefaultBinaryProbeBytes = 8000

// NormalizePath 校验并归一化一条仓库相对路径（§3.3）。
func NormalizePath(p string) (string, error) {
	return codec.ValidatePath(p)
}
