// Package budget admits the pool's runners into the machine's coreslot budget of CPU cores.
//
// It is the only package that imports coreslot. It owns the settings file and its path, the
// coreslot context a pool inherits when it is started inside `coreslot run`, the coreslot client
// and its error policy, the detector of a wedged admission section, and the report of a full
// budget.
//
// The package is Unix-only, as coreslot's library is: everything but [State] carries the unix
// build constraint. State has none, so that the packages that report it build everywhere.
package budget
