-- Sample data for examples/shop.yaml.
INSERT INTO users (id, name, email) VALUES (1, 'ann', 'ann@x'), (2, 'bob', NULL), (3, 'cat', 'cat@x');
INSERT INTO orders VALUES
  (10, 1, 'open', 5, '2026-01-02'),
  (11, 1, 'paid', 7, '2026-01-03'),
  (12, 1, 'open', 9, '2026-01-04'),
  (13, 2, 'open', 11, '2026-01-05');
INSERT INTO items VALUES (10, 1, 'pen', 2), (10, 2, 'ink', 1), (12, 1, 'pad', 3);
