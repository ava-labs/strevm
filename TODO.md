* [ ] Stop using empty blocks, instead calling `nextChunk()` in `execute()` until at correct timestamp
* [ ] Restart from last-accepted block
* [ ] Genesis block at non-zero time
* [ ] Pre-genesis pseudo chunks (same root with no receipts)
* [ ] Arbitrary inter-block period
  * [ ] Chunk recovery from database (with GC?)
* [ ] "Preference" chunk for building blocks at `+d` time
  * [ ] Chunk cloning
* [ ] Build latest block off preference instead of last-accepted
* [ ] Return transactions to mempool on `Reject()`
* [ ] Database compatibility
* [ ] APIs
* [ ] "TODO" comments in code
* [ ] ... ?