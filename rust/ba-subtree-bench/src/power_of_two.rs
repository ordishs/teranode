//! Power-of-two helpers matching `go-subtree/power_of_two.go`.

/// Matches `NextPowerOfTwo`: returns `n` if already a power of two, else the next
/// power of two above it. Integer implementation (no float log2) — equivalent to
/// the Go version for all `n >= 1`.
pub fn next_power_of_two(n: usize) -> usize {
    if n == 0 {
        return 1;
    }
    if n & (n - 1) == 0 {
        return n;
    }
    let mut p = 1usize;
    while p < n {
        p <<= 1;
    }
    p
}

#[cfg(test)]
mod tests {
    use super::next_power_of_two;
    #[test]
    fn matches_go_cases() {
        assert_eq!(next_power_of_two(1), 1);
        assert_eq!(next_power_of_two(2), 2);
        assert_eq!(next_power_of_two(3), 4);
        assert_eq!(next_power_of_two(4), 4);
        assert_eq!(next_power_of_two(7), 8);
        assert_eq!(next_power_of_two(1000), 1024);
        assert_eq!(next_power_of_two(1024), 1024);
    }
}
