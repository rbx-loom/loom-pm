package pkgname

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const corpusPath = "../../testdata/conformance/package-name.json"

type corpus struct {
	Parse []struct {
		Text  string `json:"text"`
		Valid bool   `json:"valid"`
		Scope string `json:"scope"`
		Name  string `json:"name"`
		Note  string `json:"note"`
	} `json:"parse"`

	Compare []struct {
		Left     string `json:"left"`
		Right    string `json:"right"`
		Ordering int    `json:"ordering"`
		Note     string `json:"note"`
	} `json:"compare"`
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

func because(note string) string {
	if note == "" {
		return ""
	}
	return " — " + note
}

func TestParse(t *testing.T) {
	for _, testCase := range loadCorpus(t).Parse {
		t.Run(testCase.Text, func(t *testing.T) {
			parsed, err := Parse(testCase.Text)
			if !testCase.Valid {
				if err == nil {
					t.Errorf("Parse(%q) returned %v, want an error%s", testCase.Text, parsed, because(testCase.Note))
				}
				return
			}

			if err != nil {
				t.Fatalf("Parse(%q) failed with %v, want a name%s", testCase.Text, err, because(testCase.Note))
			}

			if parsed.Scope() != testCase.Scope {
				t.Errorf("Parse(%q).Scope() = %q, want %q", testCase.Text, parsed.Scope(), testCase.Scope)
			}

			if parsed.Name() != testCase.Name {
				t.Errorf("Parse(%q).Name() = %q, want %q", testCase.Text, parsed.Name(), testCase.Name)
			}

			if parsed.String() != testCase.Text {
				t.Errorf("Parse(%q).String() = %q, want the text back%s", testCase.Text, parsed.String(), because(testCase.Note))
			}

			if parsed.IsScoped() != (testCase.Scope != "") {
				t.Errorf("Parse(%q).IsScoped() = %t, want %t", testCase.Text, parsed.IsScoped(), testCase.Scope != "")
			}
		})
	}
}

func TestCompare(t *testing.T) {
	for _, testCase := range loadCorpus(t).Compare {
		t.Run(testCase.Left+" vs "+testCase.Right, func(t *testing.T) {
			left, err := Parse(testCase.Left)
			if err != nil {
				t.Fatalf("Parse(%q): %v", testCase.Left, err)
			}

			right, err := Parse(testCase.Right)
			if err != nil {
				t.Fatalf("Parse(%q): %v", testCase.Right, err)
			}

			if got := sign(left.Compare(right)); got != testCase.Ordering {
				t.Errorf("%q.Compare(%q) = %d, want %d%s", testCase.Left, testCase.Right, got, testCase.Ordering, because(testCase.Note))
			}

			if got := sign(right.Compare(left)); got != -testCase.Ordering {
				t.Errorf("%q.Compare(%q) = %d, want %d (comparison is not antisymmetric)", testCase.Right, testCase.Left, got, -testCase.Ordering)
			}

			if want := testCase.Ordering == 0; left.Equal(right) != want {
				t.Errorf("%q.Equal(%q) = %t, want %t%s", testCase.Left, testCase.Right, !want, want, because(testCase.Note))
			}
		})
	}
}

// Normalisation and squat keys are registry concepts with no counterpart in Loom.Config,
// so they are tested here rather than in the shared corpus.

func TestNormalized(t *testing.T) {
	cases := []struct{ text, want string }{
		{"math", "math"},
		{"Math", "math"},
		{"MATH", "math"},
		{"Scope/Math", "scope/math"},
		{"math-2", "math-2"},
		{"math_2", "math_2"},
	}

	for _, testCase := range cases {
		t.Run(testCase.text, func(t *testing.T) {
			parsed, err := Parse(testCase.text)
			if err != nil {
				t.Fatalf("Parse(%q): %v", testCase.text, err)
			}

			if got := parsed.Normalized(); got != testCase.want {
				t.Errorf("Normalized() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestSquatKey(t *testing.T) {
	// names that must not both be registrable, because they are indistinguishable at a
	// glance and PackageName permits both characters
	collisions := [][2]string{
		{"my-thing", "my_thing"},
		{"My-Thing", "my_thing"},
		{"scope/a-b", "scope/a_b"},
	}

	for _, collision := range collisions {
		t.Run(collision[0]+" / "+collision[1], func(t *testing.T) {
			left, err := Parse(collision[0])
			if err != nil {
				t.Fatalf("Parse(%q): %v", collision[0], err)
			}

			right, err := Parse(collision[1])
			if err != nil {
				t.Fatalf("Parse(%q): %v", collision[1], err)
			}

			if left.SquatKey() != right.SquatKey() {
				t.Errorf("SquatKey() = %q and %q, want them to collide", left.SquatKey(), right.SquatKey())
			}
		})
	}

	distinct := [][2]string{
		{"math", "maths"},
		{"scope/math", "other/math"},
		{"math", "scope/math"},
	}

	for _, pair := range distinct {
		t.Run(pair[0]+" / "+pair[1], func(t *testing.T) {
			left, err := Parse(pair[0])
			if err != nil {
				t.Fatalf("Parse(%q): %v", pair[0], err)
			}

			right, err := Parse(pair[1])
			if err != nil {
				t.Fatalf("Parse(%q): %v", pair[1], err)
			}

			if left.SquatKey() == right.SquatKey() {
				t.Errorf("SquatKey() = %q for both, want them distinct", left.SquatKey())
			}
		})
	}
}

func TestSortIsDeterministic(t *testing.T) {
	texts := []string{"scope/b", "math", "Scope/a", "alpha", "b/x"}

	names := make([]Name, 0, len(texts))
	for _, text := range texts {
		parsed, err := Parse(text)
		if err != nil {
			t.Fatalf("Parse(%q): %v", text, err)
		}
		names = append(names, parsed)
	}

	slices.SortFunc(names, Name.Compare)

	want := []string{"alpha", "math", "b/x", "Scope/a", "scope/b"}
	for index, expected := range want {
		if got := names[index].String(); got != expected {
			t.Errorf("sorted[%d] = %q, want %q", index, got, expected)
		}
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
