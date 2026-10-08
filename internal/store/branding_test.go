package store

import "testing"

func TestBrandingDefaultEmpty(t *testing.T) {
	s := mustStore(t)
	br, err := s.ReadBranding()
	if err != nil {
		t.Fatalf("ReadBranding: %v", err)
	}
	if br.CompanyName != "" || br.HasFavicon {
		t.Errorf("fresh store branding = %+v, want zero", br)
	}
}

func TestCompanyNameRoundtrip(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteCompanyName("  Acme Corp  "); err != nil {
		t.Fatalf("WriteCompanyName: %v", err)
	}
	br, err := s.ReadBranding()
	if err != nil {
		t.Fatalf("ReadBranding: %v", err)
	}
	if br.CompanyName != "Acme Corp" {
		t.Errorf("CompanyName = %q, want trimmed %q", br.CompanyName, "Acme Corp")
	}
	// Clearing reverts to empty without disturbing the favicon flag.
	if err := s.WriteCompanyName(""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	br, _ = s.ReadBranding()
	if br.CompanyName != "" {
		t.Errorf("after clear CompanyName = %q, want empty", br.CompanyName)
	}
}

func TestFaviconAssetsRoundtripAndPreservesName(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteCompanyName("Acme"); err != nil {
		t.Fatalf("name: %v", err)
	}
	assets := map[string][]byte{
		"favicon.ico":  []byte("ICO"),
		"icon-32.png":  []byte("PNG32"),
		"icon-180.png": []byte("PNG180"),
		"icon-512.png": []byte("PNG512"),
	}
	if err := s.WriteFaviconAssets(assets); err != nil {
		t.Fatalf("WriteFaviconAssets: %v", err)
	}
	br, _ := s.ReadBranding()
	if !br.HasFavicon {
		t.Error("HasFavicon = false after write")
	}
	if br.CompanyName != "Acme" {
		t.Errorf("favicon write clobbered name: %q", br.CompanyName)
	}
	got, mime, err := s.ReadFaviconAsset("favicon.ico")
	if err != nil {
		t.Fatalf("ReadFaviconAsset: %v", err)
	}
	if string(got) != "ICO" || mime != "image/x-icon" {
		t.Errorf("favicon.ico = %q (%s)", got, mime)
	}

	// Unknown / traversal names never resolve.
	if _, _, err := s.ReadFaviconAsset("../branding.yaml"); err != ErrNotFound {
		t.Errorf("traversal name err = %v, want ErrNotFound", err)
	}

	// Remove clears the flag and the files but keeps the name.
	if err := s.RemoveFavicon(); err != nil {
		t.Fatalf("RemoveFavicon: %v", err)
	}
	br, _ = s.ReadBranding()
	if br.HasFavicon {
		t.Error("HasFavicon still true after remove")
	}
	if br.CompanyName != "Acme" {
		t.Errorf("remove clobbered name: %q", br.CompanyName)
	}
	if _, _, err := s.ReadFaviconAsset("favicon.ico"); err != ErrNotFound {
		t.Errorf("asset still present after remove: %v", err)
	}
}

func TestWriteFaviconAssetsRejectsUnknownName(t *testing.T) {
	s := mustStore(t)
	err := s.WriteFaviconAssets(map[string][]byte{"evil.sh": []byte("x")})
	if err == nil {
		t.Fatal("expected error for unknown asset name")
	}
}
