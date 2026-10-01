// Copyright 2026 The nakama-mysql contributors. Licensed under Apache-2.0.
package server

// MySQL text/decimal columns scan into []byte. JavaScript must receive text,
// not an array of byte values. Keep JSON (like pgx) and actual binary columns as bytes, and leave
// numeric, NULL and time values with their driver-provided representation.
func runtimeSQLMySQLValue(databaseType string, value any) any {
	b, ok := value.([]byte)
	if !ok {
		return value
	}
	switch databaseType {
	case "CHAR", "VARCHAR", "TEXT", "TINYTEXT", "MEDIUMTEXT", "LONGTEXT", "DECIMAL", "ENUM", "SET", "TIME":
		return string(b)
	default:
		return value
	}
}
