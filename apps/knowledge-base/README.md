# knowledge-base

The knowledge base: articles people search, read and link to tickets (docs/apps-platform.md §8.3).

First-party ServiceLater app. Same structure and install path as any third-party app (see §11 of the MVP doc).

Articles themselves are data people write in ServiceLater, not definitions: this app ships who looks after them.

- **Role** `knowledge-manager`: publishes the articles writers send for review, changes and retires published ones,
  and reads internal articles. Agents already write drafts and read internal articles; admins can do everything.

How an article moves: a writer drafts it and sends it for review; a knowledge manager publishes it (or sends it back);
a published article is later retired, and can be published again. Every save keeps a version.
