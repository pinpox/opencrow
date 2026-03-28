{ runCommand, sqlite }:
runCommand "opencrow-extension-reminders"
  {
    src = ../extensions/reminders;
    inherit sqlite;
  }
  ''
    mkdir -p $out
    cp -r $src/* $out/
    substituteInPlace $out/index.ts \
      --replace-fail '"@@SQLITE_BIN@@"' "\"$sqlite/bin/sqlite3\""
  ''
