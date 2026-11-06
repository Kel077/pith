package client

// make helper writes an ignored Go source file that sets this value at build
// time. An editable resource manifest is never used as a runtime trust anchor.
var bundledHelperIdentity string
