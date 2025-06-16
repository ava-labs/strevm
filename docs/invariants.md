# Invariants

## State transitions

This section deals with the in-memory state of a running node, the on-disk persistence of a chain, and how they relate to one another with respect to timing and data equivalence.
The EVM-specific concept of state (e.g. accounts) is orthogonal to the topics in this document.

1. Timing details the ordering guarantees that exist between, for example, in-memory objects and their equivalents on disk.
2. Data equivalence details how in-memory objects can be recovered from disk after a restart.

### Guiding principles

1. Wherever possible, reuse Ethereum-native concepts i.f.f. there is a clear rationale for equivalence.
   1. Conversely, introducing a new concept is preferred to ambiguous overloading of an existing one.
2. Timing guarantees SHOULD be sufficient to avoid race conditions if relied upon and, if insufficient, MUST be documented as such.

### Height mapping

#### Background

The `rawdb` package allows for arbitrary blocks to be stored, only coupling height and hash for canonical blocks; i.e. those in the chain history.
The canonical block with the greatest height is typically stored as the "head".
Ethereum consensus has delayed finality and `rawdb` caters for storage of the last-finalized block.

#### Mapping

| SAE      | `rawdb`   |
| -------- | --------- |
| Accepted | Canonical |
| Executed | Head      |
| Settled  | Finalized |

#### Rationale

Accepted and canonical are effectively identical concepts, save for nuanced differences between the consensus mechanisms.

From an API perspective, we treat "latest" (i.e. the chain head) as the last-executed block.
Mirroring this on disk allows for simple integration with the upstream API implementations.

Although we don't support the "finalized" block label via APIs (using "safe" for settled blocks), the `rawdb` support is otherwise unused in SAE chains.
As (a) there is no existing, alternative database concept (e.g. `rawdb` support for the latest safe block), and (b) there will be no overloading of concepts (see *Guiding principles*), we reserve `rawdb` functionality intended for the finalized block for the last settled block.

> [!NOTE]
> These also provide an unambigous inverse, allowing for recovery from disk.

### Ordering guarantees

The goal of these guarantees is to avoid race conditions.
These guarantees are not concerned with low-level races typically protected against with locks and atomics, but with the sort that arise due to the interplay of system components like consensus, the execution stream, and APIs.

> [!NOTE]
> This document is the source of truth and if the code differs there is a bug until proven otherwise.
> If it is impossible to correct the bug then this document MUST be updated to reflect reality, and an audit of potential race conditions SHOULD be performed.

#### Definitions

|        | |
| ------ | -
| $B_n$  | Block at height $n$
| $A$    | Set of Accepted blocks
| $E$    | Set of Executed blocks
| $S$    | Set of Settled blocks
| $C$    | Arbitrary condition; e.g. $B_n \in E$
| $D(C)$ | Disk artefacts of some $C$
| $M(C)$ | Memory artefacts of some $C$
| $I(C)$ | Internal indicator of some $C$
| $X(C)$ | eXternal indicator of some $C$
| $G \implies P$ | Some condition $G$ guarantees another condition $P$

An internal indicator is any signal of state that can only be accessed by the SAE implementation.
Examples include the last-accepted, -executed, and -settled block pointers.

An external indicator is any signal of state that can be accessed outside of the SAE implementation, even if in the same process.
An example is a chain-head subscription, be it in the same binary or over a websocket API.

#### Guarantees

> [!TIP]
> Guarantees should be thought of as "if I witness the guarantor $G$ then I can assume some prerequisite $P$".
> This must not be confused with cause and effect as it is the *temporal inverse*: if $G$ guarantees $P$ then $t_P < t_G$ i.e. $P$ *happens before* $G$ in Golang terminology.

| Guarantor   | Prerequisite       | Notes |
| ----------- | ------------------ | ----- |
| $B_n \in S$ | $B_n \in E$        | Settlement after execution
| $B_n \in E$ | $B_n \in A$        | Execution after settlement
| $X(C)$      | $I(C)$             | External indicator after internal indicator
| $I(C)$      | $M(C)$             | Internal indicator after memory
| $M(C)$      | $D(C)$             | Memory after disk
| $D(C)$      | $C$                | Disk after condition
| $f(B_n)$    | $f(B_{n-1})$       | See below

> [!WARNING]
> No atomic guarantees are provided with respect to different blocks entering different states.
> Of note is the lack of atomicity when updating the last-accepted and last-settled internal indicators even though settlement of a block only occurs with acceptance of some later block.

Realisation of any condition $f(\cdot)$ of a block MUST occur after the *same* condition of the parent block.
Importantly, this means that code MAY be batched in a way that interleaves state transitions of *different* types.

For example, all blocks settled by acceptance of another block MAY have their in-memory state updated *before* the last-settled block pointer is changed.
In this case, $M(B_n \in S) \implies M(B_{n-1} \in S)$ (e.g. calls to `Block.MarkSettled()` are strictly ordered) but nothing can be inferred about $I(B_{n-1} \in S)$ until $I(B_n \in S)$ is witnessed (e.g. updating the last-settled pointer MAY occur after all the calls to `MarkSettled()`).

#### Examples

1. Simple: if `blocks.Block.Executed()` returns `true` then the block's receipts can be read from the database because $M(B_n \in E) \implies D(B_n \in E)$.
2. More complex: if the atomic pointer to the last-executed block (an internal indicator) is at height $\ge n$ then the post-execution state of block $n$ can be opened because $I(B_{k \ge n} \in E) \implies M(B_k \in E) \implies M(B_n \in E)$.