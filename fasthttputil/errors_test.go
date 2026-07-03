package fasthttputil

import "testing"

func TestExportedErrorStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "ErrInmemoryListenerClosed",
			err:  ErrInmemoryListenerClosed,
			want: "fasthttputil: inmemorylistener is already closed: use of closed network connection",
		},
		{
			name: "ErrConnectionClosed",
			err:  ErrConnectionClosed,
			want: "fasthttputil: connection closed",
		},
		{
			name: "ErrTimeout",
			err:  ErrTimeout,
			want: "fasthttputil: timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.err.Error(); got != tt.want {
				t.Fatalf("unexpected error string:\ngot  %q\nwant %q", got, tt.want)
			}
		})
	}
}
