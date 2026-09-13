package auth

import "time"

// timeNow exists so TOTP tests read the same clock the library does.
func timeNow() time.Time { return time.Now() }
