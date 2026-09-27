-- Test tables for the Definition panel: one table per DDL feature, plus a view.
-- Run in the SQL console of the target database (or: make dev, then paste). Re-runnable.
DROP VIEW IF EXISTS dbo.order_totals;
DROP TABLE IF EXISTS dbo.order_line;
DROP TABLE IF EXISTS dbo.[order];
DROP TABLE IF EXISTS dbo.customer;
DROP TABLE IF EXISTS dbo.event_log;
DROP TABLE IF EXISTS dbo.odd_types;
DROP TYPE IF EXISTS dbo.email_t;

CREATE TYPE dbo.email_t FROM NVARCHAR(254) NULL;

-- identity, named and unnamed defaults, collation, unique constraint, filtered + included index.
-- EXEC because the batch is compiled before dbo.email_t exists; the console has no GO.
EXEC('CREATE TABLE dbo.customer (
  id       INT IDENTITY(1000, 10) NOT NULL CONSTRAINT PK_customer PRIMARY KEY CLUSTERED,
  name     NVARCHAR(100) COLLATE Latin1_General_CS_AS NOT NULL,
  email    dbo.email_t NULL,
  country  CHAR(2) NOT NULL CONSTRAINT DF_customer_country DEFAULT ''CH'',
  active   BIT NOT NULL DEFAULT 1,
  created  DATETIME2(3) NOT NULL DEFAULT SYSUTCDATETIME(),
  CONSTRAINT UQ_customer_email UNIQUE NONCLUSTERED (email),
  CONSTRAINT CK_customer_country CHECK (country = UPPER(country))
)');
CREATE NONCLUSTERED INDEX IX_customer_active_name ON dbo.customer (name DESC) INCLUDE (country) WHERE active = 1;

-- composite nonclustered PK, computed columns (persisted and not), FK with cascade rules
CREATE TABLE dbo.[order] (
  customer_id INT NOT NULL,
  order_no    INT NOT NULL,
  placed      DATE NOT NULL DEFAULT GETDATE(),
  net         DECIMAL(10,2) NOT NULL,
  vat_rate    DECIMAL(4,3) NOT NULL DEFAULT 0.081,
  gross       AS (net * (1 + vat_rate)) PERSISTED,
  year        AS DATEPART(YEAR, placed),
  CONSTRAINT PK_order PRIMARY KEY NONCLUSTERED (customer_id, order_no),
  CONSTRAINT FK_order_customer FOREIGN KEY (customer_id) REFERENCES dbo.customer (id) ON DELETE CASCADE ON UPDATE NO ACTION,
  CONSTRAINT CK_order_net CHECK (net >= 0)
);
CREATE UNIQUE CLUSTERED INDEX CX_order_placed ON dbo.[order] (placed, customer_id, order_no);

-- composite FK with SET NULL, unique index (not a constraint), multiple FKs
CREATE TABLE dbo.order_line (
  id          BIGINT IDENTITY PRIMARY KEY,
  customer_id INT NULL,
  order_no    INT NULL,
  sku         VARCHAR(20) NOT NULL,
  qty         SMALLINT NOT NULL CHECK (qty > 0),
  CONSTRAINT FK_line_order FOREIGN KEY (customer_id, order_no) REFERENCES dbo.[order] (customer_id, order_no) ON DELETE SET NULL,
  CONSTRAINT FK_line_customer FOREIGN KEY (customer_id) REFERENCES dbo.customer (id)
);
CREATE UNIQUE NONCLUSTERED INDEX UX_line_order_sku ON dbo.order_line (customer_id, order_no, sku);

-- heap: no primary key, no indexes at all
CREATE TABLE dbo.event_log (
  at      DATETIMEOFFSET(0) NOT NULL DEFAULT SYSDATETIMEOFFSET(),
  level   TINYINT NOT NULL,
  message NVARCHAR(MAX) NULL
);

-- types with length/precision edge cases and read-only columns
CREATE TABLE dbo.odd_types (
  id      UNIQUEIDENTIFIER NOT NULL DEFAULT NEWSEQUENTIALID() PRIMARY KEY,
  fixed   NCHAR(8) NULL,
  raw     VARBINARY(MAX) NULL,
  hash    BINARY(32) NULL,
  ts      ROWVERSION,
  ratio   NUMERIC(18,6) NULL,
  clock   TIME(0) NULL,
  amount  SMALLMONEY NULL,
  doc     XML NULL,
  point   GEOGRAPHY NULL,
  misc    SQL_VARIANT NULL
);

-- CREATE VIEW must be alone in its batch; EXEC keeps the script a single batch for the console.
EXEC('CREATE VIEW dbo.order_totals AS
  SELECT c.name, COUNT(*) AS orders, SUM(o.gross) AS gross
  FROM dbo.customer c JOIN dbo.[order] o ON o.customer_id = c.id
  GROUP BY c.name');

INSERT INTO dbo.customer (name, email, country) VALUES ('Alice', 'alice@example.com', 'CH'), ('Bob', NULL, 'DE');
INSERT INTO dbo.[order] (customer_id, order_no, net) VALUES (1000, 1, 100), (1000, 2, 250.50), (1010, 1, 42);
INSERT INTO dbo.order_line (customer_id, order_no, sku, qty) VALUES (1000, 1, 'A-1', 2), (1000, 1, 'B-7', 1), (1010, 1, 'A-1', 5);
INSERT INTO dbo.event_log (level, message) VALUES (1, 'started'), (2, NULL);
