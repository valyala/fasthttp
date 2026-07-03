package prefork

import "testing"

func TestExportedErrorStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "ErrOverRecovery",
			err:  ErrOverRecovery,
			want: "prefork: exceeding the value of recoverthreshold",
		},
		{
			name: "ErrOnlyReuseportOnWindows",
			err:  ErrOnlyReuseportOnWindows,
			want: "prefork: windows only supports reuseport = true",
		},
		{
			name: "ErrCommandProducerNilCmd",
			err:  ErrCommandProducerNilCmd,
			want: "prefork: commandproducer returned nil command",
		},
		{
			name: "ErrCommandProducerNotStarted",
			err:  ErrCommandProducerNotStarted,
			want: "prefork: commandproducer must return a started command",
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
