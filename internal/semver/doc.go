// Package semver ports Loom.Config.Version and Loom.Config.VersionRequirement from
// github.com/rbx-loom/loom.
//
// It is a port rather than a dependency because Loom's requirements are not ordinary
// semver: a requirement is exactly one interval with no "||" union, an unsatisfiable
// requirement is a parse error rather than an empty set, and a prerelease is accepted
// only when one of the requirement's own bounds names a prerelease of the same
// major.minor.patch. A library disagreeing on any of those would let the registry accept
// a dependency at publish time that the client rejects at resolve time.
//
// testdata/conformance/semver.json is checked into both repositories and executed by both
// test suites. The C# side is the reference implementation: when the two disagree, C# is
// right unless the disagreement is a bug there, in which case fix it there and add the
// case. Never change this package to make a case pass without deciding which side is
// wrong first — the two halves disagreeing about what "^1.2" means surfaces as committed
// lock files pinning the wrong version, not as an error anyone sees.
package semver
