package pesig

import (
	"bytes"
	"debug/pe"
	"testing"

	"github.com/FemLed/masseuse-camlink/internal/pesig/petest"
)

// The synthetic image is a PE the standard library agrees about, so what
// the tests below say about offsets is said about a real header layout.
func TestSyntheticImageIsAPE(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []petest.Option
	}{{"pe32+", nil}, {"pe32", []petest.Option{petest.PE32()}}} {
		t.Run(tc.name, func(t *testing.T) {
			img := petest.Image([]byte("hello"), tc.opts...)
			f, err := pe.NewFile(bytes.NewReader(img))
			if err != nil {
				t.Fatalf("debug/pe: %v", err)
			}
			if len(f.Sections) != 1 || f.Sections[0].Name != ".text" {
				t.Fatalf("sections: %+v", f.Sections)
			}
			h, err := ParseBytes(img)
			if err != nil {
				t.Fatal(err)
			}
			if h.Signed() {
				t.Fatal("an unsigned image reads as signed")
			}
			if h.ImageEnd != int64(len(img)) {
				t.Fatalf("ImageEnd %d, file %d", h.ImageEnd, len(img))
			}
			if h.PE32Plus == (tc.name == "pe32") {
				t.Fatalf("PE32Plus %v for %s", h.PE32Plus, tc.name)
			}
			if h.SecurityEntryOffset == 0 {
				t.Fatal("no security entry located")
			}
			// The checksum offset agrees with debug/pe's view of the header.
			var want uint32
			switch oh := f.OptionalHeader.(type) {
			case *pe.OptionalHeader64:
				want = oh.CheckSum
			case *pe.OptionalHeader32:
				want = oh.CheckSum
			}
			if want != 0 {
				t.Fatalf("checksum %d in a fresh image", want)
			}
		})
	}
}

func TestStripRemovesASignatureAndItsPadding(t *testing.T) {
	for _, n := range []int{5, 8, 13} { // section sizes: file ends unaligned, aligned, unaligned
		data := bytes.Repeat([]byte{7}, n)
		bare := petest.Image(data)
		signed := petest.Sign(bare, 100, 0xdeadbeef)
		h, err := ParseBytes(signed)
		if err != nil {
			t.Fatal(err)
		}
		if !h.Signed() || int64(h.SecurityOffset)%FooterAlign != 0 {
			t.Fatalf("n=%d: header %+v", n, h)
		}
		if h.End(int64(len(signed))) != int64(h.SecurityOffset) {
			t.Fatalf("n=%d: End %d, table at %d", n, h.End(int64(len(signed))), h.SecurityOffset)
		}
		got, err := Strip(signed)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !bytes.Equal(got, bare) {
			t.Fatalf("n=%d: stripped %d bytes, bare %d; padding or fields left behind", n, len(got), len(bare))
		}
		// debug/pe reads the stripped file as unsigned.
		f, err := pe.NewFile(bytes.NewReader(got))
		if err != nil {
			t.Fatal(err)
		}
		if oh, ok := f.OptionalHeader.(*pe.OptionalHeader64); !ok || oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY].Size != 0 || oh.CheckSum != 0 {
			t.Fatalf("n=%d: the security entry or checksum was not zeroed", n)
		}
	}
}

func TestStripNormalizesAnUnsignedFile(t *testing.T) {
	bare := petest.Image([]byte("abc"))
	got, err := Strip(bare)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bare) {
		t.Fatal("an unsigned Go-shaped image did not pass through unchanged")
	}
	// A linker-written checksum is zeroed on both sides of a comparison.
	withSum := petest.Image([]byte("abc"), petest.Checksum(0x1234))
	got, err = Strip(withSum)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bare) {
		t.Fatal("the checksum was not zeroed on the unsigned side")
	}
	signed := petest.Sign(withSum, 64, 0x9999)
	got2, err := Strip(signed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, got) {
		t.Fatal("Strip(signed) differs from Strip(unsigned) for a file with a linker checksum")
	}
}

func TestStripKeepsAnOverlayThatIsNotPadding(t *testing.T) {
	bare := petest.Image([]byte("abc"))
	payload := []byte("PAYLOAD-DATA-0123456789abcdef") // more than FooterAlign, not zeros
	withOverlay := append(append([]byte(nil), bare...), payload...)
	signed := petest.Sign(withOverlay, 40, 1)
	got, err := Strip(signed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, bare) || !bytes.Equal(got[len(bare):len(bare)+len(payload)], payload) {
		t.Fatal("the overlay was cut")
	}
	// What follows the overlay is at most the alignment padding.
	if extra := len(got) - len(withOverlay); extra < 0 || extra >= FooterAlign {
		t.Fatalf("%d bytes past the overlay", extra)
	}
	// A zero overlay cannot be told from padding, so it is kept, padding
	// and all: only exactly the bytes that align the image are cut.
	eightZeros := append(append([]byte(nil), bare...), make([]byte, 8)...)
	got, err = Strip(petest.Sign(eightZeros, 40, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, eightZeros) || len(got)-len(eightZeros) >= FooterAlign {
		t.Fatalf("a zero overlay was cut: %d bytes of %d", len(got), len(eightZeros))
	}
	// An image that ends on the boundary gets no padding, so nothing after
	// it is ever taken for padding.
	aligned := petest.Image(bytes.Repeat([]byte{1}, 8))
	if len(aligned)%FooterAlign != 0 {
		t.Fatalf("test image not aligned: %d", len(aligned))
	}
	oneZero := append(append([]byte(nil), aligned...), 0)
	got, err = Strip(petest.Sign(oneZero, 40, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < len(oneZero) {
		t.Fatal("a one-byte overlay after an aligned image was taken for padding")
	}
}

func TestStripRefusesATableNotAtTheEnd(t *testing.T) {
	signed := petest.Sign(petest.Image([]byte("abc")), 40, 1)
	trailing := append(append([]byte(nil), signed...), []byte("junk")...)
	if _, err := Strip(trailing); err == nil {
		t.Fatal("a certificate table followed by more data was stripped")
	}
}

func TestParseRefusesWhatIsNotAPE(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("short"), bytes.Repeat([]byte("x"), 200), append([]byte("MZ"), make([]byte, 100)...)} {
		if _, err := ParseBytes(data); err == nil {
			t.Fatalf("%q parsed as a PE", data)
		}
	}
	// A PE whose certificate table points outside the file.
	signed := petest.Sign(petest.Image([]byte("abc")), 40, 1)
	truncated := signed[:len(signed)-10]
	if _, err := ParseBytes(truncated); err == nil {
		t.Fatal("a truncated certificate table parsed")
	}
}
