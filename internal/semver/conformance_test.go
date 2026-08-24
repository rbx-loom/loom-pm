package semver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// corpusPath is the shared conformance corpus, checked into this repository and into
// rbx-loom/loom, and executed by both test suites. See doc.go.
const corpusPath = "../../testdata/conformance/semver.json"

type corpus struct {
	ParseVersion []struct {
		Text  string `json:"text"`
		Valid bool   `json:"valid"`
		Note  string `json:"note"`
	} `json:"parse_version"`

	Compare []struct {
		Left     string `json:"left"`
		Right    string `json:"right"`
		Ordering int    `json:"ordering"`
		Note     string `json:"note"`
	} `json:"compare"`

	Sort []struct {
		Input    []string `json:"input"`
		Expected []string `json:"expected"`
		Note     string   `json:"note"`
	} `json:"sort"`

	ParseRequirement []struct {
		Text  string `json:"text"`
		Valid bool   `json:"valid"`
		Note  string `json:"note"`
	} `json:"parse_requirement"`

	Satisfies []struct {
		Requirement string `json:"requirement"`
		Version     string `json:"version"`
		Satisfied   bool   `json:"satisfied"`
		Note        string `json:"note"`
	} `json:"satisfies"`

	Intersect []struct {
		Requirements []string `json:"requirements"`
		Satisfiable  bool     `json:"satisfiable"`
		Comparator   string   `json:"comparator"`
		Note         string   `json:"note"`
	} `json:"intersect"`
}

func loadCorpus(t *testing.T) corpus {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(corpusPath))
	if err != nil {
		t.Fatalf("reading the conformance corpus: %v", err)
	}

	var loaded corpus
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("parsing the conformance corpus: %v", err)
	}

	return loaded
}

// because reports the corpus's note alongside a failure, so a broken case explains the
// rule it is defending rather than only the values it expected.
func because(note string) string {
	if note == "" {
		return ""
	}
	return " — " + note
}

func TestParseVersion(t *testing.T) {
	for _, testCase := range loadCorpus(t).ParseVersion {
		t.Run(testCase.Text, func(t *testing.T) {
			parsed, err := ParseVersion(testCase.Text)
			switch {
			case testCase.Valid && err != nil:
				t.Errorf("ParseVersion(%q) failed with %v, want a version%s", testCase.Text, err, because(testCase.Note))
			case !testCase.Valid && err == nil:
				t.Errorf("ParseVersion(%q) returned %v, want an error%s", testCase.Text, parsed, because(testCase.Note))
			}
		})
	}
}

// TestVersionRoundTrip checks that String renders back what was parsed, build metadata
// included — String is the only place metadata appears.
func TestVersionRoundTrip(t *testing.T) {
	for _, testCase := range loadCorpus(t).ParseVersion {
		if !testCase.Valid {
			continue
		}

		t.Run(testCase.Text, func(t *testing.T) {
			parsed, err := ParseVersion(testCase.Text)
			if err != nil {
				t.Fatalf("ParseVersion(%q): %v", testCase.Text, err)
			}

			if got := parsed.String(); got != testCase.Text {
				t.Errorf("ParseVersion(%q).String() = %q, want %q", testCase.Text, got, testCase.Text)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	for _, testCase := range loadCorpus(t).Compare {
		t.Run(testCase.Left+" vs "+testCase.Right, func(t *testing.T) {
			left, err := ParseVersion(testCase.Left)
			if err != nil {
				t.Fatalf("ParseVersion(%q): %v", testCase.Left, err)
			}

			right, err := ParseVersion(testCase.Right)
			if err != nil {
				t.Fatalf("ParseVersion(%q): %v", testCase.Right, err)
			}

			if got := sign(left.Compare(right)); got != testCase.Ordering {
				t.Errorf("%q.Compare(%q) = %d, want %d%s", testCase.Left, testCase.Right, got, testCase.Ordering, because(testCase.Note))
			}

			// comparison must be antisymmetric, which is cheap to check and catches a
			// whole class of half-finished prerelease logic
			if got := sign(right.Compare(left)); got != -testCase.Ordering {
				t.Errorf("%q.Compare(%q) = %d, want %d (comparison is not antisymmetric)", testCase.Right, testCase.Left, got, -testCase.Ordering)
			}

			// Equal must agree with Compare == 0, including for the build-metadata cases
			if want := testCase.Ordering == 0; left.Equal(right) != want {
				t.Errorf("%q.Equal(%q) = %t, want %t%s", testCase.Left, testCase.Right, !want, want, because(testCase.Note))
			}
		})
	}
}

func TestSortVersions(t *testing.T) {
	for _, testCase := range loadCorpus(t).Sort {
		t.Run("", func(t *testing.T) {
			versions := make([]Version, 0, len(testCase.Input))
			for _, text := range testCase.Input {
				parsed, err := ParseVersion(text)
				if err != nil {
					t.Fatalf("ParseVersion(%q): %v", text, err)
				}
				versions = append(versions, parsed)
			}

			SortVersions(versions)

			if len(versions) != len(testCase.Expected) {
				t.Fatalf("SortVersions() returned %d versions, want %d", len(versions), len(testCase.Expected))
			}

			for index, want := range testCase.Expected {
				if got := versions[index].String(); got != want {
					t.Errorf("SortVersions()[%d] = %q, want %q%s", index, got, want, because(testCase.Note))
				}
			}
		})
	}
}

func TestParseRequirement(t *testing.T) {
	for _, testCase := range loadCorpus(t).ParseRequirement {
		t.Run(testCase.Text, func(t *testing.T) {
			parsed, err := ParseRequirement(testCase.Text)
			switch {
			case testCase.Valid && err != nil:
				t.Errorf("ParseRequirement(%q) failed with %v, want a requirement%s", testCase.Text, err, because(testCase.Note))
			case !testCase.Valid && err == nil:
				t.Errorf("ParseRequirement(%q) returned %v, want an error%s", testCase.Text, parsed, because(testCase.Note))
			}
		})
	}
}

func TestSatisfies(t *testing.T) {
	for _, testCase := range loadCorpus(t).Satisfies {
		t.Run(testCase.Requirement+" / "+testCase.Version, func(t *testing.T) {
			requirement, err := ParseRequirement(testCase.Requirement)
			if err != nil {
				t.Fatalf("ParseRequirement(%q): %v", testCase.Requirement, err)
			}

			version, err := ParseVersion(testCase.Version)
			if err != nil {
				t.Fatalf("ParseVersion(%q): %v", testCase.Version, err)
			}

			if got := requirement.Satisfies(version); got != testCase.Satisfied {
				t.Errorf("%q.Satisfies(%q) = %t, want %t%s", testCase.Requirement, testCase.Version, got, testCase.Satisfied, because(testCase.Note))
			}
		})
	}
}

func TestIntersect(t *testing.T) {
	for _, testCase := range loadCorpus(t).Intersect {
		t.Run("", func(t *testing.T) {
			requirements := make([]Requirement, 0, len(testCase.Requirements))
			for _, text := range testCase.Requirements {
				parsed, err := ParseRequirement(text)
				if err != nil {
					t.Fatalf("ParseRequirement(%q): %v", text, err)
				}
				requirements = append(requirements, parsed)
			}

			intersected, satisfiable := IntersectAll(requirements)
			if satisfiable != testCase.Satisfiable {
				t.Fatalf("IntersectAll(%v) satisfiable = %t, want %t%s", testCase.Requirements, satisfiable, testCase.Satisfiable, because(testCase.Note))
			}

			if !testCase.Satisfiable {
				return
			}

			if got := intersected.ComparatorString(); got != testCase.Comparator {
				t.Errorf("IntersectAll(%v).ComparatorString() = %q, want %q%s", testCase.Requirements, got, testCase.Comparator, because(testCase.Note))
			}
		})
	}
}

func sign(value int) int {
	switch {
	case value < 0:
		return -1
	case value > 0:
		return 1
	default:
		return 0
	}
}
