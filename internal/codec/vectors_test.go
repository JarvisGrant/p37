// Package codec_test 冻结并校验附录 A.5 的编码黄金向量。
//
// 这些向量是**格式资产**：一经冻结不得更改。任何变化都是格式破坏，必须升
// format_ver 并提供迁移（§2.2）。
package codec_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/version"
)

var update = flag.Bool("update", false, "重新生成并冻结黄金向量")

const vectorsPath = "testdata/vectors/encoding.json"

// VectorSet 是冻结文件的结构。
type VectorSet struct {
	Algo     string   `json:"algo"`
	FrozenAt string   `json:"frozen_at"`
	Note     string   `json:"note"`
	Cases    []Vector `json:"cases"`
}

// Vector 是一个向量。
type Vector struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Input       string `json:"input"`
	EncodingHex string `json:"encoding_hex"`
	ID          string `json:"id"`
}

// build 构造全部向量。生成与校验共用同一份代码，避免「期望值」被手写错。
func build() VectorSet {
	type c struct {
		name, desc, input string
		enc               []byte
		id                hash.ID
	}
	add := func(name, desc, input string, enc []byte) c {
		return c{name: name, desc: desc, input: input, enc: enc, id: hash.Sum(enc)}
	}

	var cases []c

	// --- Blob：id 直接是原文哈希，无编码 ---
	cases = append(cases, add("blob_empty", "空分片：id = BLAKE3(空字节)", "raw = (empty)", []byte{}))

	// --- FileContent ---
	cases = append(cases, add("filecontent_1chunk",
		"单 chunk 文件：total=11, eol=lf, chunks=[BLAKE3(\"hello world\")]",
		`total_size=11 eol=lf chunks=[blob("hello world")]`,
		(&tree.FileContent{
			TotalSize: 11,
			EOL:       codec.EOLlf,
			Chunks:    []hash.ID{hash.Sum([]byte("hello world"))},
		}).Encode()))

	cases = append(cases, add("filecontent_3chunk",
		"三 chunk 文件：total=30, eol=crlf, chunks=[a,b,c]",
		`total_size=30 eol=crlf chunks=[blob("a"), blob("b"), blob("c")]`,
		(&tree.FileContent{
			TotalSize: 30,
			EOL:       codec.EOLcrlf,
			Chunks: []hash.ID{
				hash.Sum([]byte("a")), hash.Sum([]byte("b")), hash.Sum([]byte("c")),
			},
		}).Encode()))

	cases = append(cases, add("filecontent_empty_file",
		"空文件：0 个 chunk（附录 H.5）",
		`total_size=0 eol=lf chunks=[]`,
		(&tree.FileContent{TotalSize: 0, EOL: codec.EOLlf}).Encode()))

	// --- Tree ---
	cases = append(cases, add("tree_empty", "空树（v0 的根）", "entries=[]",
		(&tree.Tree{}).Encode()))

	fcID := hash.Sum([]byte("file-content"))
	cases = append(cases, add("tree_flat3",
		"三个平级文件，按字节序排序",
		`entries=[Dir(Tree), a.txt(File,0644), b.go(File,0755)]`,
		(&tree.Tree{Entries: []tree.Entry{
			{Name: "b.go", Mode: codec.ModeExecutable, Kind: codec.EntryFile, ID: fcID},
			{Name: "a.txt", Mode: codec.ModeNormal, Kind: codec.EntryFile, ID: fcID},
			{Name: "Dir", Mode: codec.ModeNormal, Kind: codec.EntryTree, ID: fcID},
		}}).Encode()))

	cases = append(cases, add("tree_symlink",
		"含符号链接的树：mode 恒为 0",
		`entries=[link(Symlink -> blob("target"))]`,
		(&tree.Tree{Entries: []tree.Entry{
			{Name: "link", Mode: codec.ModeNormal, Kind: codec.EntrySymlink,
				ID: hash.Sum([]byte("target"))},
		}}).Encode()))

	// NFC 归一化：显式构造 NFD 形式（e + U+0301），编码后必须与 NFC 一致。
	nfd := "cafe\u0301" // café 的 NFD
	cases = append(cases, add("tree_unicode_nfc",
		"路径做 NFC 归一化：输入为 NFD 时编码结果与 NFC 相同（§3.3）",
		`entries=[name="caf\u00e9"(NFC)\u2190\u8f93\u5165\u4e3a NFD "cafe\u0301"]`,
		(&tree.Tree{Entries: []tree.Entry{
			{Name: nfd, Mode: codec.ModeNormal, Kind: codec.EntryFile, ID: fcID},
		}}).Encode()))

	cases = append(cases, add("tree_case_sensitive",
		"大小写敏感排序：Foo.go(0x46) 排在 foo.go(0x66) 之前（§3.3）",
		`entries=["Foo.go","foo.go"]`,
		(&tree.Tree{Entries: []tree.Entry{
			{Name: "foo.go", Mode: codec.ModeNormal, Kind: codec.EntryFile, ID: fcID},
			{Name: "Foo.go", Mode: codec.ModeNormal, Kind: codec.EntryFile, ID: fcID},
		}}).Encode()))

	// --- Version ---
	treeID := hash.Sum([]byte("root-tree"))
	cases = append(cases, add("version_first",
		"首版本：parent 为全零 32 字节（附录 A.3）",
		`parent=0x00*32 tree=root-tree epoch=0 agent="p37-import" task="import" ts=0 msg=""`,
		version.New(hash.Zero, treeID, 0, "p37-import", "import", 0, "").Encode()))

	cases = append(cases, add("version_normal",
		"普通版本：含 epoch / agent / task_hash / ts / msg",
		`parent=H(v_prev) tree=root-tree epoch=3 agent="agent-A" task="实现登录" ts=1700000000000 msg="feat: login"`,
		version.New(hash.Sum([]byte("v_prev")), treeID, 3, "agent-A",
			"实现登录", 1700000000000, "feat: login").Encode()))

	var vs []Vector
	for _, ca := range cases {
		vs = append(vs, Vector{
			Name:        ca.name,
			Description: ca.desc,
			Input:       ca.input,
			EncodingHex: fmt.Sprintf("%x", ca.enc),
			ID:          ca.id.String(),
		})
	}

	return VectorSet{
		Algo:     "blake3-256",
		FrozenAt: "M1",
		Note:     "格式资产：任何变化都是格式破坏，必须升 format_ver 并提供迁移（§2.2、附录 A.5）",
		Cases:    vs,
	}
}

// TestGoldenVectors 校验编码向量与冻结文件一致。
func TestGoldenVectors(t *testing.T) {
	got := build()

	if *update {
		if err := os.MkdirAll(filepath.Dir(vectorsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorsPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("已重新冻结编码向量 (%d 个): %s", len(got.Cases), vectorsPath)
		return
	}

	b, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("读取冻结向量失败（首次运行请加 -update）: %v", err)
	}
	var want VectorSet
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if len(got.Cases) != len(want.Cases) {
		t.Fatalf("向量数 %d != 冻结的 %d", len(got.Cases), len(want.Cases))
	}
	for i := range got.Cases {
		g, w := got.Cases[i], want.Cases[i]
		if g.Name != w.Name {
			t.Fatalf("第 %d 个向量名 %q != %q", i, g.Name, w.Name)
		}
		if g.ID != w.ID {
			t.Errorf("向量 %s 的 id 已偏离冻结值 —— 这是格式破坏\n got: %s\nwant: %s",
				g.Name, g.ID, w.ID)
		}
		if g.EncodingHex != w.EncodingHex {
			t.Errorf("向量 %s 的编码字节已偏离冻结值\n got: %s\nwant: %s",
				g.Name, g.EncodingHex, w.EncodingHex)
		}
	}
}

// TestNFCNormalizationEquivalence 断言 NFD 与 NFC 输入产生同一 id（§3.3）。
func TestNFCNormalizationEquivalence(t *testing.T) {
	fcID := hash.Sum([]byte("x"))
	nfc := "caf\u00e9"
	nfd := "cafe\u0301"
	if nfc == nfd {
		t.Skip("编译器已归一化字符串字面量，跳过")
	}
	a := (&tree.Tree{Entries: []tree.Entry{{Name: nfc, Kind: codec.EntryFile, ID: fcID}}}).ID()
	b := (&tree.Tree{Entries: []tree.Entry{{Name: nfd, Kind: codec.EntryFile, ID: fcID}}}).ID()
	if a != b {
		t.Fatalf("NFD 与 NFC 路径产生不同 id: %s vs %s", a, b)
	}
}

// TestDecodeRoundTrip 断言编码 → 解码 → 编码逐字节相同（§附录 L.1 #2）。
func TestDecodeRoundTrip(t *testing.T) {
	fc := &tree.FileContent{TotalSize: 42, EOL: codec.EOLmixed,
		Chunks: []hash.ID{hash.Sum([]byte("p")), hash.Sum([]byte("q"))}}
	enc := fc.Encode()
	dec, err := tree.DecodeFileContent(enc)
	if err != nil {
		t.Fatal(err)
	}
	if string(dec.Encode()) != string(enc) {
		t.Fatal("FileContent 编解码不往返")
	}

	tr := &tree.Tree{Entries: []tree.Entry{
		{Name: "z", Mode: codec.ModeExecutable, Kind: codec.EntryFile, ID: hash.Sum([]byte("1"))},
		{Name: "a", Mode: codec.ModeNormal, Kind: codec.EntryTree, ID: hash.Sum([]byte("2"))},
	}}
	encT := tr.Encode()
	decT, err := tree.DecodeTree(encT)
	if err != nil {
		t.Fatal(err)
	}
	if string(decT.Encode()) != string(encT) {
		t.Fatal("Tree 编解码不往返")
	}

	encV := version.New(hash.Zero, hash.Sum([]byte("t")), 7, "agent", "task", 123456789, "msg").Encode()
	decV, err := version.Decode(encV)
	if err != nil {
		t.Fatal(err)
	}
	if string(decV.Encode()) != string(encV) {
		t.Fatal("Version 编解码不往返")
	}
}

// TestDecodeRejectsInvalid 覆盖解码侧的健壮性约束（§13）。
func TestDecodeRejectsInvalid(t *testing.T) {
	// 错误类型号。
	if _, err := tree.DecodeTree([]byte{codec.TypeFileContent, 0}); err == nil {
		t.Fatal("类型号不符应报错")
	}
	// 条目未排序。
	e := codec.NewEncoder()
	e.U8(codec.TypeTree)
	e.Uvarint(2)
	for _, n := range []string{"b", "a"} {
		e.Str(n)
		e.Uvarint(codec.ModeNormal)
		e.U8(codec.EntryFile)
		e.Hash(hash.Sum([]byte(n)))
	}
	if _, err := tree.DecodeTree(e.Out()); err == nil {
		t.Fatal("未排序的树应被拒绝")
	}
	// 重复条目。
	e2 := codec.NewEncoder()
	e2.U8(codec.TypeTree)
	e2.Uvarint(2)
	for i := 0; i < 2; i++ {
		e2.Str("same")
		e2.Uvarint(codec.ModeNormal)
		e2.U8(codec.EntryFile)
		e2.Hash(hash.Sum([]byte("x")))
	}
	if _, err := tree.DecodeTree(e2.Out()); err == nil {
		t.Fatal("重复条目应被拒绝")
	}
	// 条目名含 '/'。
	e3 := codec.NewEncoder()
	e3.U8(codec.TypeTree)
	e3.Uvarint(1)
	e3.Str("a/b")
	e3.Uvarint(codec.ModeNormal)
	e3.U8(codec.EntryFile)
	e3.Hash(hash.Sum([]byte("x")))
	if _, err := tree.DecodeTree(e3.Out()); err == nil {
		t.Fatal("含 / 的条目名应被拒绝")
	}
	// 尾部多余字节。
	e4 := codec.NewEncoder()
	e4.U8(codec.TypeTree)
	e4.Uvarint(0)
	e4.U8(0xff)
	if _, err := tree.DecodeTree(e4.Out()); err == nil {
		t.Fatal("尾部多余字节应被拒绝")
	}
}

// TestValidatePath 覆盖 §3.3 的路径规则。
func TestValidatePath(t *testing.T) {
	ok := []string{"a", "a/b", "src/auth/token.go", "文件.go"}
	for _, p := range ok {
		if _, err := codec.ValidatePath(p); err != nil {
			t.Errorf("应接受 %q: %v", p, err)
		}
	}
	bad := []string{"", "/a", "a/", "a//b", "a/./b", "a/../b", "..", "."}
	for _, p := range bad {
		if _, err := codec.ValidatePath(p); err == nil {
			t.Errorf("应拒绝 %q", p)
		}
	}
}
