## 2025-05-30 - String Concatenation Bottleneck in Local Markdown Parsing
**Learning:** In Go, repeatedly using `+=` inside a loop for string concatenation creates O(N²) allocations, which is heavily problematic when parsing large files like ADRs line by line in `splitSections`.
**Action:** Always use `strings.Builder` for accumulating string blocks in line-by-line parsing loops to ensure linear performance.
