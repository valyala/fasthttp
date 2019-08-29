package fasthttputil

import "testing"

func TestStringSlices(t *testing.T) {
	slices := AcquireStringSlices()
	defer ReleaseStringSlices(slices)

	bytess := [][]byte{
		[]byte("Lorem ipsum dolor sit amet, consectetur adipiscing elit"),
		[]byte("sed do eiusmod tempor incididunt ut labore et dolore magna aliqua"),
		[]byte(`Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris
				nisi ut aliquip ex ea commodo consequat.
				Duis aute irure dolor in reprehenderit in voluptate velit esse cillum
				dolore eu fugiat nulla pariatur. Excepteur sint occaecat cupidatat non proident,
				sunt in culpa qui officia deserunt mollit anim id est laborum`),
		[]byte("Sed ut perspiciatis"),
		[]byte("sed quia consequuntur magni dolores eos qui ratione voluptatem sequi nesciunt"),
		[]byte("Ut enim ad minima veniam, quis nostrum exercitationem ullam corporis suscipit"),
		[]byte(""),
		[]byte("laboriosam, nisi ut aliquid ex ea commodi consequatur"),
		[]byte("Quis autem vel eum iure reprehenderit qui in ea voluptate velit esse quam nihil molestiae consequatur"),
		[]byte("vel illum qui dolorem eum fugiat quo voluptas nulla pariatur"),
	}
	total := len(bytess)

	for _, bytes := range bytess {
		slices.WriteBytes(bytes)
	}
	if number := slices.Number(); total != number {
		t.Fatalf("want: %d, have: %d", total, number)
	}

	for idx, bytes := range bytess {
		want := string(bytes)
		have, got := slices.NextStringSlice()
		if have != want || !got {
			t.Fatalf("want: %s, have: %s, got: %v", want, have, got)
		}

		remain := slices.Remain()
		if remain != total-idx-1 {
			t.Fatalf("want: %d, have: %d", total-idx-1, remain)
		}
	}

	have, got := slices.NextStringSlice()
	if have != "" || got {
		t.Fatalf("want: cannot get, have: %s, got: %v", have, got)
	}

	err := slices.LastError()
	if err != nil {
		t.Fatal(err)
	}
}

func TestEmptyStringSlices(t *testing.T) {
	slices := AcquireStringSlices()
	defer ReleaseStringSlices(slices)

	have, got := slices.NextStringSlice()
	if have != "" || got {
		t.Fatalf("want: cannot get, have: %s, got: %v", have, got)
	}
	if remain := slices.Remain(); remain != 0 {
		t.Fatalf("want: %d, have: %d", 0, remain)
	}
	if number := slices.Number(); number != 0 {
		t.Fatalf("want: %d, have: %d", 0, number)
	}
}
