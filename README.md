# csv2mariadb

[![Go](https://img.shields.io/badge/Go-1.27+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![MariaDB](https://img.shields.io/badge/MariaDB%20%2F%20MySQL-compatible-003545?logo=mariadb&logoColor=white)](https://mariadb.org/)
[![Dependencies](https://img.shields.io/badge/dependencies-none-brightgreen)](#how-it-works)

**Import German-style CSV exports (Excel, DATEV, ERP systems …) into a MariaDB table with one command.**

`csv2mariadb` reads a semicolon-separated CSV file, creates the table from the header line (or loads into an
existing one) and inserts all rows in a single transaction. It understands German number and date formats,
handles the usual encodings (UTF-8, UTF-16, Windows-1252) and ships as **one static binary built only from the Go standard library**:
no database driver, no runtime dependencies.

```console
$ csv2mariadb -infer -use-pk "Kunden-Nr." kunden.csv
Created table `mydb`.`kunden` (6 columns)
Imported 2 rows into `mydb`.`kunden` in 41ms
```

---

## Features

- **Zero dependencies**: a single Go file with a built-in client for the MariaDB/MySQL wire protocol.
- **Automatic encoding detection**: UTF-8 (with or without BOM), UTF-16 LE/BE with BOM, Windows-1252 fallback.
- **Table created from the header**: column names are normalised (`Straße` → `Strasse`, `Erstellt am` → `Erstellt_am`)
  and the original header is kept as a column `COMMENT`.
- **Type inference** (`-infer`) for German formats:
  | CSV value                         | Column type     |
  |-----------------------------------|-----------------|
  | `4711`, `-1.234.567`              | `BIGINT`        |
  | `1.234,56`, `99,5`                | `DECIMAL(p,s)`  |
  | `31.12.2025`, `2025-12-31`        | `DATE`          |
  | `31.12.2025 14:30`, ISO date-time | `DATETIME`      |
  | anything else (≤ 255 chars)       | `VARCHAR(255)`  |
  | longer text                       | `TEXT`          |

  Numbers with leading zeros (postal codes, `0815`) are kept as text.
- **Safe loading**: everything runs in one transaction, so a failed import rolls back completely. `-if-exists replace`
  loads into a staging table and swaps it in atomically with `RENAME TABLE`.
- **Primary keys**: add an `AUTO_INCREMENT` key (`-pk id`) or promote existing CSV columns, including composite keys (`-use-pk`).
- **Dry run**: inspect detected encoding, column mapping, types and the `CREATE TABLE` statement without touching the database.
- **Strict by default**: the session runs in a strict SQL mode, so invalid data fails loudly instead of being silently mangled.
  Server warnings are reported with the affected CSV line numbers.
- **TLS and Unix sockets** are supported.

## Installation

Requires Go 1.27 or newer.

```sh
git clone https://github.com/hgrimm/csv2mariadb.git
cd csv2mariadb
go build -o csv2mariadb .
```

Cross-compile for a Linux server from any machine:

```sh
GOOS=linux GOARCH=amd64 go build -o csv2mariadb .
```

## Configuration

Database credentials are kept out of the command line and read from a config file. Start from the example:

```sh
cp csv2mariadb.conf.example csv2mariadb.conf
chmod 600 csv2mariadb.conf
```

```ini
host      = 127.0.0.1
port      = 3306
#socket   = /run/mysqld/mysqld.sock   # if set, used instead of host/port
user      = importer
password  = secret
database  = mydb

# TLS: false | true (verify certificate) | skip-verify
tls       = false
#tls_ca   = /etc/mysql/ca.pem

# Collation for new tables and the connection (must be utf8mb4_*)
collation = utf8mb4_unicode_ci

# Connect/handshake timeout (seconds or Go duration such as 15s)
timeout   = 10
```

The config file is searched in this order:

1. the path given with `-config <path>`
2. `./csv2mariadb.conf`
3. `~/.csv2mariadb.conf`
4. `csv2mariadb.conf` next to the executable

The environment variable `CSV2MARIADB_PASSWORD` overrides `password`. The tool warns if a config file containing
a password is readable by other users.

## Usage

```
csv2mariadb [flags] file.csv
```

The table name defaults to the file name without extension (`kunden.csv` → `kunden`). Flags may appear before
or after the file name.

### Examples

Preview what would be created (no database connection needed):

```console
$ csv2mariadb -dry-run -infer -use-pk "Kunden-Nr." kunden.csv
File:      kunden.csv (encoding utf8, separator ';')
Table:     kunden
Rows:      2 (0 empty rows skipped)

Columns:
    1  Kunden-Nr.                     -> Kunden-Nr.                     BIGINT  (primary key)
    2  Name                           -> Name                           VARCHAR(255)
    3  Straße                         -> Strasse                        VARCHAR(255)
    4  PLZ                            -> PLZ                            VARCHAR(255)
    5  Umsatz €                       -> Umsatz_€                       DECIMAL(6,2)
    6  Erstellt am                    -> Erstellt_am                    DATE

CREATE TABLE `kunden` (
  `Kunden-Nr.` BIGINT NOT NULL,
  `Name` VARCHAR(255) NULL,
  `Strasse` VARCHAR(255) NULL COMMENT 'Straße',
  `PLZ` VARCHAR(255) NULL,
  `Umsatz_€` DECIMAL(6,2) NULL COMMENT 'Umsatz €',
  `Erstellt_am` DATE NULL COMMENT 'Erstellt am',
  PRIMARY KEY (`Kunden-Nr.`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

Import with type detection and an auto-increment key:

```sh
csv2mariadb -infer -pk id customers.csv
```

Refresh a table from a nightly export (atomic swap, readers never see a half-loaded table):

```sh
csv2mariadb -infer -if-exists replace -table customers export_2025-12-31.csv
```

Append to an existing table (columns are matched by name, values converted to the existing column types):

```sh
csv2mariadb -if-exists append bookings_january.csv
```

Tab-separated Windows-1252 file with sloppy quoting:

```sh
csv2mariadb -sep tab -encoding cp1252 -lazy-quotes -lenient legacy.txt
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-table name` | file name | Target table name |
| `-config file` | *(search path)* | Configuration file with the MariaDB access data |
| `-if-exists mode` | `fail` | If the table exists: `fail`, `append`, `truncate` or `replace` |
| `-infer` | off | Detect `BIGINT`, `DECIMAL`, `DATE`, `DATETIME` instead of storing everything as `TEXT` |
| `-pk column` | | Add an `AUTO_INCREMENT` primary key column with this name |
| `-use-pk cols` | | Use existing CSV column(s) as primary key, comma-separated for a composite key |
| `-encoding enc` | `auto` | `auto`, `utf8`, `utf16le`, `utf16be` or `cp1252` |
| `-sep char` | `;` | Field separator: a single character, or `tab` |
| `-lazy-quotes` | off | Tolerate stray quotes inside fields |
| `-lenient` | off | Fill missing trailing fields of short rows with `NULL` instead of failing |
| `-ascii` | off | Replace every character outside `[A-Za-z0-9_]` in names with `_` |
| `-empty-string` | off | Store empty text fields as `''` instead of `NULL` |
| `-batch rows` | `1000` | Maximum rows per `INSERT` statement |
| `-dry-run` | off | Only analyse the file and print the `CREATE TABLE` statement |

`-pk` and `-use-pk` only apply when a table is created (new table or `replace`).

#### `-if-exists` modes

| Mode | Behaviour |
|------|-----------|
| `fail` | Abort if the table already exists |
| `append` | Insert rows into the existing table |
| `truncate` | Delete all rows, then insert, within the same transaction |
| `replace` | Build a new table from the CSV and atomically swap it in with `RENAME TABLE` |

## How it works

1. **Read the header**: detect the encoding from the BOM (or validate UTF-8, else fall back to Windows-1252) and
   normalise column names: umlauts → `ae`/`oe`/`ue`, `ß` → `ss`, whitespace → `_`.
2. **Analyse** (only with `-infer`, `-use-pk` or `-dry-run`): a first pass over the file collects per-column statistics
   and picks the narrowest fitting type.
3. **Connect**: speaks the MariaDB/MySQL protocol directly (handshake, `mysql_native_password`, optional TLS), sets
   `utf8mb4` and a strict SQL mode, and reads `max_allowed_packet` to size the batches.
4. **Load**: a second pass converts values (`1.234,56` → `1234.56`, `31.12.2025` → `2025-12-31`) and sends multi-row
   `INSERT` statements inside a single transaction.

### Supported authentication

| Plugin | Supported |
|--------|-----------|
| `mysql_native_password` | ✅ |
| `unix_socket` / `auth_socket` | ✅ (via `socket`) |
| `mysql_clear_password` | ✅ only over TLS or a Unix socket |
| `client_ed25519` | ❌ use an account with `mysql_native_password` |

To create a suitable import user:

```sql
CREATE USER 'importer'@'%' IDENTIFIED VIA mysql_native_password USING PASSWORD('secret');
GRANT SELECT, INSERT, DELETE, CREATE, DROP, ALTER ON mydb.* TO 'importer'@'%';
```

(`DROP` and `ALTER` are only needed for `-if-exists replace`, `DELETE` only for `truncate`.)

## Input format

- Separator `;` by default, fields optionally enclosed in double quotes (`"…"`, with `""` as an escaped quote)
- CRLF or LF line endings
- First line = column names
- Completely empty rows are skipped
