package actions

import "testing"

// The two account actions judge the same name differently, and which one a
// caller reaches for is the whole point of having both.
//
// The administrator the installer creates is called admin. Owning a site means
// having a Linux account, and sending that through the customer action made
// the first site on every fresh install fail -- complaining about the name the
// installer itself had just chosen.
func TestAccountNameRulesDifferByPath(t *testing.T) {
	// A customer may not take a name that makes them look like staff.
	for _, name := range []string{"admin", "test", "user", "guest"} {
		if err := (&AccountRequest{Username: name}).Validate(); err == nil {
			t.Errorf("a customer was allowed the name %q", name)
		}
	}
	// An operator asking for their own login is the opposite case.
	for _, name := range []string{"admin", "carol", "a_b-1"} {
		if err := (&StaffAccountRequest{Username: name}).Validate(); err != nil {
			t.Errorf("an operator was refused their own name %q: %v", name, err)
		}
	}
	// The customer path may never name something the panel must not act as.
	//
	// The staff path deliberately does not check a list here: an operator's
	// name is judged against the passwd file by linuxuser.CreateStaff, which
	// refuses any existing account below the system uid threshold. A list
	// would be the weaker test of the two -- it can only refuse the names
	// somebody thought to write down.
	for _, name := range []string{"root", "mysql", "apache", "opanel", "", "Bad"} {
		if err := (&AccountRequest{Username: name}).Validate(); err == nil {
			t.Errorf("the customer path accepted %q", name)
		}
	}
}
