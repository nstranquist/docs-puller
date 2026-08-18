# PostgreSQL indexes

PostgreSQL supports B-tree, hash, GiST, SP-GiST, GIN, and BRIN indexes. B-tree
is the default for equality and range comparisons. GIN is useful for values
that contain multiple components, such as arrays, JSONB documents, and text
search vectors.
