package cli

import (
	"errors"
	"strings"
	"testing"
)

// Colima with experimental on reported `docker checkpoint` as available, and it was not: the
// dump succeeds there and every restore fails. The row has to agree with what the command does.
func TestDoctorCheckpointRowAgreesWithTheCommand(t *testing.T) {
	vm := errors.New("memory checkpoint needs a Linux host")

	if c := checkpointCapability("true", vm); c.Have {
		t.Fatalf("doctor reports checkpoint available on a host where it is refused: %+v", c)
	} else if !strings.Contains(c.Detail, "Linux") {
		t.Errorf("the row does not say why: %+v", c)
	}

	if c := checkpointCapability("true", nil); !c.Have {
		t.Fatalf("a Linux host with experimental on is reported without checkpoint: %+v", c)
	}

	if c := checkpointCapability("false", nil); c.Have {
		t.Fatalf("experimental=false is reported as able to checkpoint: %+v", c)
	}
}
