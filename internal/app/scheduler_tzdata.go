package app

// Native cron schedules must behave the same on hosts that do not install a
// system zoneinfo database (for example, minimal Linux images). Keeping the
// embedded database at the Scheduler domain boundary also makes every binary
// that can validate or run a native schedule carry the same IANA data.
import _ "time/tzdata"
