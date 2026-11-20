package server

import "testing"

func TestValidateMusicLayout(t *testing.T) {
	tests := []struct {
		public, users string
		valid         bool
	}{
		{"/music/public", "/music/users", true},
		{"/mnt/music/public", "/mnt/music/users", true},
		{"/music", "/music/users", false},
		{"/music/users", "/music/public", false},
		{"/music/public", "/music/public/users", false},
		{"music/public", "/music/users", false},
		{"/music/public", "", false},
		{"/music/public", "/elsewhere/users", false},
	}
	for _, tc := range tests {
		err := validateMusicLayout(tc.public, tc.users)
		if (err == nil) != tc.valid {
			t.Errorf("layout(%q,%q) error=%v valid=%t", tc.public, tc.users, err, tc.valid)
		}
	}
}
