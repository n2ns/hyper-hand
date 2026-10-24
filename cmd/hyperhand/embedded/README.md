Build output only: the release workflow places `hyperhand-agent.exe` here before building `hyperhand.exe`, which
embeds it, so the single `hyperhand.exe` can install the guest agent without a file beside it. Development builds put
the agent beside `hyperhand.exe` instead. Delete an agent left here after a local release build: every later build
would embed it, and `TestPlainBuildEmbedsNoAgent` fails. The directory is tracked for this note; `*.exe` is ignored.
