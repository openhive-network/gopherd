package auth

// MinAccountNameLength / MaxAccountNameLength mirror
// HIVE_MIN_ACCOUNT_NAME_LENGTH (3) / HIVE_MAX_ACCOUNT_NAME_LENGTH (16)
// from libraries/protocol/include/hive/protocol/config.hpp.
const (
	MinAccountNameLength = 3
	MaxAccountNameLength = 16
)

// AccountNameValidity mirrors hive::protocol::account_name_validity.
type AccountNameValidity int

const (
	AccountNameValid AccountNameValidity = iota
	AccountNameTooShort
	AccountNameTooLong
	AccountNameInvalidSequence
)

// CheckAccountName ports hive::protocol::detail::check_account_name
// (libraries/protocol/authority.cpp:58). An account name is composed of
// dot-separated segments where each segment is 3-N chars, starts with a
// lowercase letter, ends with [a-z0-9], and inner chars are [a-z0-9-].
// Total length is bounded by [MIN, MAX].
func CheckAccountName(name string) AccountNameValidity {
	n := len(name)
	if n < MinAccountNameLength {
		return AccountNameTooShort
	}
	if n > MaxAccountNameLength {
		return AccountNameTooLong
	}
	begin := 0
	for {
		end := indexByteFrom(name, '.', begin)
		if end < 0 {
			end = n
		}
		if end-begin < 3 {
			return AccountNameInvalidSequence
		}
		// First char: a-z.
		if !isLowerLetter(name[begin]) {
			return AccountNameInvalidSequence
		}
		// Last char: a-z0-9.
		if !isLowerLetter(name[end-1]) && !isDigit(name[end-1]) {
			return AccountNameInvalidSequence
		}
		// Inner chars: a-z0-9-.
		for i := begin + 1; i < end-1; i++ {
			c := name[i]
			if !isLowerLetter(c) && !isDigit(c) && c != '-' {
				return AccountNameInvalidSequence
			}
		}
		if end == n {
			break
		}
		begin = end + 1
	}
	return AccountNameValid
}

// IsValidAccountName mirrors hive::protocol::is_valid_account_name.
func IsValidAccountName(name string) bool {
	return CheckAccountName(name) == AccountNameValid
}

func isLowerLetter(c byte) bool { return c >= 'a' && c <= 'z' }
func isDigit(c byte) bool       { return c >= '0' && c <= '9' }

func indexByteFrom(s string, c byte, start int) int {
	for i := start; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
