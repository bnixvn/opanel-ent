//! Build metadata.
//!
//! Stamped in through the environment at compile time, falling back to the
//! crate version with a marker. The same contract the Go build has, and for
//! the same reason: a binary that cannot say which build it is turns every
//! report about it into guesswork.

pub fn string() -> String {
    match option_env!("OPANEL_VERSION") {
        Some(v) if !v.is_empty() => v.to_owned(),
        _ => format!("{}-dev", env!("CARGO_PKG_VERSION")),
    }
}
