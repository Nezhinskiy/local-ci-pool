package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"
)

const oneGiB = int64(1) << 30

func TestSlots(t *testing.T) {
	cases := []struct {
		name string
		mem  int64
		cpus int
		want int
	}{
		{"10 GiB, 10 CPUs", 10 * oneGiB, 10, 2},
		{"18 GiB, 10 CPUs", 18 * oneGiB, 10, 4},
		{"18 GiB, 4 CPUs", 18 * oneGiB, 4, 2},
		{"5 GiB, 10 CPUs", 5 * oneGiB, 10, 0},
		{"64 GiB, 32 CPUs", 64 * oneGiB, 32, 8},
		// Docker Desktop's VM reports MemTotal below its slider, so the memory
		// term rounds to the nearest GiB instead of flooring.
		{"9.7 GiB, 10 CPUs", 10414000000, 10, 2},
		{"7.75 GiB (8 GB slider), 10 CPUs", 8319504384, 10, 1},
		{"just under 6 GiB rounds to 6", 6*oneGiB - 1, 10, 1},
		{"5.4 GiB rounds down to 5", 5*oneGiB + oneGiB*4/10, 10, 0},
		{"memory is the limit, not CPUs", 10 * oneGiB, 64, 2},
		{"CPUs are the limit, not memory", 64 * oneGiB, 4, 2},
		{"one CPU is no slot", 64 * oneGiB, 1, 0},
		{"zero memory", 0, 10, 0},
		{"negative memory", -oneGiB, 10, 0},
		{"negative CPUs", 64 * oneGiB, -2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Slots(tc.mem, tc.cpus); got != tc.want {
				t.Fatalf("Slots(%d, %d) = %d, want %d", tc.mem, tc.cpus, got, tc.want)
			}
		})
	}
}

func TestName(t *testing.T) {
	good := map[string]string{
		"Example-MacBook": "examplemacbo",
		"examplemac":      "examplemac",
		"Example Mac 2":   "examplemac2",
		"A":               "a",
		"abcdefghijkl":    "abcdefghijkl",
		"abcdefghijklm":   "abcdefghijkl",
		"  mac\n":         "mac",
	}
	for in, want := range good {
		got, err := Name(in)
		if err != nil || got != want {
			t.Errorf("Name(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"!!!", "", "---", "\n"} {
		if got, err := Name(in); !errors.Is(err, ErrHostName) {
			t.Errorf("Name(%q) = %q, want an error", in, got)
		}
	}
}

func TestOwner(t *testing.T) {
	sum := sha256.Sum256([]byte("00000000-0000-0000-0000-000000000001"))
	want := "examplemac-" + hex.EncodeToString(sum[:])[:8]
	if got := Owner("examplemac", "00000000-0000-0000-0000-000000000001"); got != want {
		t.Fatalf("Owner = %q, want %q", got, want)
	}
	if Owner("examplemac", "00000000-0000-0000-0000-000000000002") == want {
		t.Fatal("two UUIDs gave the same owner")
	}
	if !regexp.MustCompile(`^examplemac-[0-9a-f]{8}$`).MatchString(want) {
		t.Fatalf("owner shape: %q", want)
	}
}

func TestScaleSetNameTruncates(t *testing.T) {
	const limit = 20
	got := ScaleSetName("alpha-very-long-identity", "examplemac", limit)
	if len(got) > limit {
		t.Fatalf("%q is %d characters, limit %d", got, len(got), limit)
	}
	if !regexp.MustCompile(`-[0-9a-f]{6}$`).MatchString(got) {
		t.Fatalf("%q does not end with a 6-hex suffix", got)
	}
	if again := ScaleSetName("alpha-very-long-identity", "examplemac", limit); again != got {
		t.Fatalf("not stable: %q then %q", got, again)
	}
	sum := sha256.Sum256([]byte("alpha-very-long-identity-examplemac"))
	if !strings.HasSuffix(got, hex.EncodeToString(sum[:])[:6]) {
		t.Fatalf("%q: the suffix is not the hash of the full name", got)
	}
	// Distinct full names that share a truncated prefix stay distinct.
	other := ScaleSetName("alpha-very-long-identity", "examplemad", limit)
	if other == got {
		t.Fatalf("different machines collapsed to %q", got)
	}
}

func TestScaleSetNameWithinLimitIsUntouched(t *testing.T) {
	if got := ScaleSetName("alpha", "examplemac", 64); got != "alpha-examplemac" {
		t.Fatalf("got %q", got)
	}
	exact := ScaleSetName("alpha", "examplemac", len("alpha-examplemac"))
	if exact != "alpha-examplemac" {
		t.Fatalf("a name of exactly the limit was changed: %q", exact)
	}
}

func TestScaleSetNameNeverEndsWithDoubleDash(t *testing.T) {
	// The cut falls right after a "-": the prefix must not leave "--".
	// Characters 0..12 of the full name end with "-", where the cut falls.
	got := ScaleSetName("aaaaaaaaaaaa-bbbbbbbbbbbb", "examplemac", 20)
	if strings.Contains(got, "--") || len(got) > 20 {
		t.Fatalf("%q", got)
	}
	for _, limit := range []int{1, 3, 6, 7, 8} {
		got := ScaleSetName("alpha-very-long-identity", "examplemac", limit)
		if len(got) > limit {
			t.Errorf("limit %d: %q is too long", limit, got)
		}
	}
}

func TestDetectComposesTheMachine(t *testing.T) {
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "scutil":
			if strings.Join(args, " ") != "--get LocalHostName" {
				t.Errorf("scutil args = %v", args)
			}
			return []byte("Example-MacBook\n"), nil
		case "ioreg":
			if strings.Join(args, " ") != "-rd1 -c IOPlatformExpertDevice" {
				t.Errorf("ioreg args = %v", args)
			}
			return []byte(`+-o Root  <class IORegistryEntry>
  +-o Example-MacBook  <class IOPlatformExpertDevice>
    {
      "IOPlatformSerialNumber" = "EXAMPLE0001"
      "IOPlatformUUID" = "00000000-0000-0000-0000-00000000ABCD"
    }
`), nil
		}
		t.Errorf("unexpected command %s", name)
		return nil, errors.New("unexpected")
	}
	info := func(context.Context) (Fingerprint, error) {
		return Fingerprint{ID: "docker-id", Mem: 18 * oneGiB, CPUs: 10}, nil
	}
	m, err := detect(context.Background(), run, info)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "examplemacbo" || m.Slots != 4 || m.FP != (Fingerprint{ID: "docker-id", Mem: 18 * oneGiB, CPUs: 10}) {
		t.Fatalf("machine = %+v", m)
	}
	if m.Owner != Owner("examplemacbo", "00000000-0000-0000-0000-00000000ABCD") {
		t.Fatalf("owner = %q", m.Owner)
	}
}

func TestDetectReportsEachFailure(t *testing.T) {
	okInfo := func(context.Context) (Fingerprint, error) { return Fingerprint{Mem: 18 * oneGiB, CPUs: 10}, nil }
	runWith := func(host, ioreg string, hostErr, ioErr error) func(context.Context, string, ...string) ([]byte, error) {
		return func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "scutil" {
				return []byte(host), hostErr
			}
			return []byte(ioreg), ioErr
		}
	}
	uuid := `"IOPlatformUUID" = "00000000-0000-0000-0000-00000000ABCD"`
	cases := map[string]struct {
		run  func(context.Context, string, ...string) ([]byte, error)
		info func(context.Context) (Fingerprint, error)
		want string
	}{
		"scutil fails":   {runWith("", uuid, errors.New("x"), nil), okInfo, "LocalHostName"},
		"bad host name":  {runWith("!!!", uuid, nil, nil), okInfo, "host name"},
		"ioreg fails":    {runWith("mac", "", nil, errors.New("x")), okInfo, "IOPlatformUUID"},
		"no uuid":        {runWith("mac", "nothing here", nil, nil), okInfo, "IOPlatformUUID"},
		"docker failure": {runWith("mac", uuid, nil, nil), func(context.Context) (Fingerprint, error) { return Fingerprint{}, errors.New("daemon down") }, "daemon down"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := detect(context.Background(), tc.run, tc.info)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDetectLeavesZeroSlotsToTheCaller(t *testing.T) {
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "scutil" {
			return []byte("mac"), nil
		}
		return []byte(`"IOPlatformUUID" = "00000000-0000-0000-0000-00000000ABCD"`), nil
	}
	m, err := detect(context.Background(), run, func(context.Context) (Fingerprint, error) {
		return Fingerprint{Mem: 4 * oneGiB, CPUs: 8}, nil
	})
	if err != nil || m.Slots != 0 {
		t.Fatalf("machine = %+v, %v; the supervisor refuses 0 slots, not Detect", m, err)
	}
}

func TestRunnerArch(t *testing.T) {
	good := map[string]string{"aarch64": "arm64", "arm64": "arm64", "x86_64": "x64", "amd64": "x64", "AARCH64": "arm64"}
	for in, want := range good {
		got, err := RunnerArch(in)
		if err != nil || got != want {
			t.Errorf("RunnerArch(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "armv7l", "s390x", "riscv64", "x64"} {
		if got, err := RunnerArch(in); err == nil {
			t.Errorf("RunnerArch(%q) = %q, want an error", in, got)
		}
	}
}

func TestLocalName(t *testing.T) {
	run := func(host string, err error) runFunc {
		return func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "scutil" || strings.Join(args, " ") != "--get LocalHostName" {
				t.Errorf("unexpected command %s %v", name, args)
			}
			return []byte(host), err
		}
	}
	got, err := localName(context.Background(), run("Example-MacBook\n", nil))
	if err != nil || got != "examplemacbo" {
		t.Fatalf("localName = %q, %v", got, err)
	}
	if _, err := localName(context.Background(), run("", errors.New("x"))); err == nil || !strings.Contains(err.Error(), "LocalHostName") {
		t.Fatalf("scutil failure = %v", err)
	}
	if _, err := localName(context.Background(), run("!!!\n", nil)); !errors.Is(err, ErrHostName) {
		t.Fatalf("bad host name = %v, want ErrHostName", err)
	}
}
