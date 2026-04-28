# auth package notes

Wire-format reference for hive::protocol::authority:

- `account_name_type` = `fixed_string<16>`. Despite being declared as a fixed 16-byte storage,
  its FC raw serialization is identical to `std::string`: a varint-prefixed length followed
  by the UTF-8 bytes (libraries/protocol/include/hive/protocol/fixed_string.hpp:243-256).
- `weight_type` = `uint16_t`, written little-endian.
- `public_key_type` wraps a `fc::ecc::public_key_data` = `fc::array<char, 33>`, serialized
  as 33 raw bytes (no length prefix, since fc::array is fixed-size).
- `authority` reflects to `(weight_threshold)(account_auths)(key_auths)`:
    - `weight_threshold`: uint32 LE
    - `account_auths`: flat_map<account_name_type, weight_type> — varint count + sorted
      (name, weight) pairs.
    - `key_auths`: flat_map<public_key_type, weight_type> — varint count + sorted
      (33-byte-key, weight) pairs.
