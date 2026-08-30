package features

import (
	"strings"
	"testing"
)

// withRegistry swaps the registry for one test and puts it back.
func withRegistry(t *testing.T, fs ...Feature) {
	t.Helper()

	saved := registry
	registry = nil

	t.Cleanup(func() { registry = saved })

	for _, f := range fs {
		Register(f)
	}
}

func TestAFeatureIsOffUntilItIsAskedFor(t *testing.T) {
	withRegistry(t, Feature{Name: "ssh", Stability: Preview, Summary: "an SSH service"})

	t.Setenv("SBX_FEATURES", "")

	if Enabled("ssh") {
		t.Error("a preview feature was on with SBX_FEATURES unset")
	}

	t.Setenv("SBX_FEATURES", "ssh")

	if !Enabled("ssh") {
		t.Error("naming a feature did not turn it on")
	}
}

// The list is what somebody types by hand, so it has to survive being typed by hand.
func TestTheListToleratesHowPeopleWriteIt(t *testing.T) {
	withRegistry(t,
		Feature{Name: "ssh", Stability: Preview, Summary: "s"},
		Feature{Name: "devcontainer", Stability: Preview, Summary: "d"},
	)

	for _, spec := range []string{
		"ssh,devcontainer",
		" ssh , devcontainer ",
		"ssh,,devcontainer",
		",ssh,devcontainer,",
	} {
		t.Setenv("SBX_FEATURES", spec)

		if !Enabled("ssh") || !Enabled("devcontainer") {
			t.Errorf("%q did not enable both", spec)
		}
	}
}

// `all` must NOT be a thing. Somebody who wants everything on wants each thing for a reason, and
// a blanket switch is how an unproven feature reaches a CI job nobody chose to put it in.
func TestThereIsNoSwitchThatTurnsEverythingOn(t *testing.T) {
	withRegistry(t,
		Feature{Name: "ssh", Stability: Preview, Summary: "s"},
		Feature{Name: "risky", Stability: Experimental, Summary: "r", Caveat: "unproven"},
	)

	for _, spec := range []string{"all", "*", "1", "true", "on"} {
		t.Setenv("SBX_FEATURES", spec)

		if Enabled("ssh") || Enabled("risky") {
			t.Errorf("%q turned features on wholesale", spec)
		}
	}
}

// A typo that silently does nothing is the failure this surfaces: SBX_FEATURES=devcontainers
// reads as "the feature is broken" rather than "the name has an s on the end".
func TestATypoIsReportedRatherThanIgnored(t *testing.T) {
	withRegistry(t, Feature{Name: "devcontainer", Stability: Preview, Summary: "d"})

	t.Setenv("SBX_FEATURES", "devcontainers,ssh")

	got := Unknown()
	if len(got) != 2 || got[0] != "devcontainers" || got[1] != "ssh" {
		t.Errorf("Unknown() = %v, want both unrecognised names, sorted", got)
	}

	t.Setenv("SBX_FEATURES", "devcontainer")

	if u := Unknown(); len(u) != 0 {
		t.Errorf("a correct name was reported as unknown: %v", u)
	}
}

// Turning an experimental feature on says what is unproven about it. Finding that out later from
// behaviour is worse than being told at the moment of choosing.
func TestAnExperimentalFeatureSaysWhatIsUnprovenWhenItIsUsed(t *testing.T) {
	withRegistry(t,
		Feature{Name: "risky", Stability: Experimental, Summary: "r", Caveat: "loses data on restart"},
		Feature{Name: "ssh", Stability: Preview, Summary: "s"},
	)

	t.Setenv("SBX_FEATURES", "risky,ssh")

	note := Note("risky")
	if !strings.Contains(note, "loses data on restart") {
		t.Errorf("the note does not carry the caveat: %q", note)
	}

	if n := Note("ssh"); n != "" {
		t.Errorf("a preview feature printed a caveat note: %q", n)
	}

	t.Setenv("SBX_FEATURES", "")

	if n := Note("risky"); n != "" {
		t.Errorf("a feature that is off printed a note: %q", n)
	}
}

// An experimental feature with no caveat would switch on silently, which is the one thing the
// stability field exists to prevent - so it is a build-time mistake, not a runtime surprise.
func TestAnExperimentalFeatureMustSayWhatIsUnproven(t *testing.T) {
	withRegistry(t)

	defer func() {
		if recover() == nil {
			t.Error("registered an experimental feature with no caveat")
		}
	}()

	Register(Feature{Name: "silent", Stability: Experimental, Summary: "s"})
}

// The refusal has to carry the exact thing to type. A gate that says only "not enabled" sends
// the reader off to find a variable name the message already knows.
func TestTheRefusalSaysHowToTurnItOn(t *testing.T) {
	withRegistry(t, Feature{Name: "ssh", Stability: Preview, Summary: "an SSH service in a sandbox"})

	err := Refuse("ssh")
	if err == nil {
		t.Fatal("no refusal")
	}

	for _, want := range []string{"ssh", "preview", "SBX_FEATURES=ssh", "an SSH service"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestRegistryStaysSorted(t *testing.T) {
	withRegistry(t,
		Feature{Name: "zeta", Stability: Preview, Summary: "z"},
		Feature{Name: "alpha", Stability: Preview, Summary: "a"},
		Feature{Name: "mid", Stability: Preview, Summary: "m"},
	)

	got := All()
	for i := 1; i < len(got); i++ {
		if got[i-1].Name > got[i].Name {
			t.Errorf("registry is not sorted: %v", got)
		}
	}
}
