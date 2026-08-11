// Package budget implements tiered emission against an approximate token
// budget: verdict + counts always; failed resources next; ranked evidence
// next; enumeration of healthy resources almost never. Estimation is ~4
// chars/token by design — no tokenizer dependency.
package budget
