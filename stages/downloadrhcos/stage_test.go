package downloadrhcos

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"testing"
)

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func TestUncompressKernel_EFIzboot(t *testing.T) {
	raw := append([]byte("ARMd-pretend-kernel"), bytes.Repeat([]byte{0x42}, 100)...)
	payload := gz(raw)
	// Build a minimal EFI zboot image: MZ + zimg + offset/size + "gzip".
	const off = 0x40
	hdr := make([]byte, off)
	copy(hdr[0:], []byte("MZ"))
	copy(hdr[4:], []byte("zimg"))
	binary.LittleEndian.PutUint32(hdr[8:], off)
	binary.LittleEndian.PutUint32(hdr[12:], uint32(len(payload)))
	copy(hdr[0x18:], []byte("gzip"))
	img := append(hdr, payload...)

	got, err := uncompressKernel(img)
	if err != nil {
		t.Fatalf("uncompressKernel: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("zboot payload not decompressed correctly: got %d bytes", len(got))
	}
}

func TestUncompressKernel_BareGzipAndRaw(t *testing.T) {
	raw := []byte("uncompressed-image-bytes")
	got, err := uncompressKernel(gz(raw))
	if err != nil || !bytes.Equal(got, raw) {
		t.Errorf("bare gzip: got %q err %v", got, err)
	}
	got, err = uncompressKernel(raw)
	if err != nil || !bytes.Equal(got, raw) {
		t.Errorf("raw passthrough: got %q err %v", got, err)
	}
}
