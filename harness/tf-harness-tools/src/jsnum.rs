//! JS number formatting: tool args arrive as JSON numbers, and pi
//! interpolates them into user-visible strings with `Number.prototype
//! .toString` semantics (integers print without a decimal point).

pub fn js_num(n: f64) -> String {
    if n.is_nan() {
        return "NaN".to_string();
    }
    if n.is_infinite() {
        return if n > 0.0 { "Infinity" } else { "-Infinity" }.to_string();
    }
    if n == n.trunc() && n.abs() < 9.007_199_254_740_992e15 {
        return format!("{}", n as i64);
    }
    format!("{n}")
}

/// JS ToIntegerOrInfinity truncation, for array-index coercion.
pub fn to_index(n: f64) -> usize {
    if n.is_nan() || n <= 0.0 {
        0
    } else {
        n.trunc() as usize
    }
}
