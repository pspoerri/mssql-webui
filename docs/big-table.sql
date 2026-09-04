-- Test data: dbo.big with 20 columns of mixed types and 2000 rows.
-- Run in the SQL console of the target database (or: make dev, then paste).
CREATE TABLE dbo.big (
  id         INT IDENTITY PRIMARY KEY,
  name       NVARCHAR(50) NOT NULL,
  email      NVARCHAR(100) NULL,
  city       NVARCHAR(50) NULL,
  country    CHAR(2) NULL,
  age        TINYINT NULL,
  score      SMALLINT NULL,
  visits     INT NULL,
  big_number BIGINT NULL,
  price      DECIMAL(10,2) NULL,
  ratio      FLOAT NULL,
  weight     REAL NULL,
  active     BIT NOT NULL DEFAULT 1,
  created    DATETIME2 NULL,
  birthday   DATE NULL,
  login_time TIME NULL,
  updated    DATETIMEOFFSET NULL,
  uid        UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID(),
  notes      NVARCHAR(MAX) NULL,
  amount     MONEY NULL
);

WITH n AS (
  SELECT TOP (2000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i
  FROM sys.all_objects a CROSS JOIN sys.all_objects b
)
INSERT INTO dbo.big (name, email, city, country, age, score, visits, big_number, price, ratio, weight,
                     active, created, birthday, login_time, updated, notes, amount)
SELECT
  CONCAT('User ', i),
  CASE WHEN i % 7 = 0 THEN NULL ELSE CONCAT('user', i, '@example.com') END,
  CHOOSE(i % 5 + 1, N'Zürich', N'Bern', N'Basel', N'Genève', N'Lugano'),
  CHOOSE(i % 3 + 1, 'CH', 'DE', 'FR'),
  18 + i % 60,
  (i * 37) % 1000,
  i * 3,
  CAST(i AS BIGINT) * 1000000007,
  CAST(i AS DECIMAL(10,2)) / 7,
  i / 3.0,
  i * 0.5,
  i % 2,
  DATEADD(MINUTE, i, '2024-01-01'),
  DATEADD(DAY, i, '1970-01-01'),
  DATEADD(SECOND, i * 43, CAST('00:00:00' AS TIME)),
  SYSDATETIMEOFFSET(),
  CASE WHEN i % 10 = 0 THEN CONCAT('Note for row ', i, ': lorem ipsum dolor sit amet') ELSE NULL END,
  i * 1.25
FROM n;
