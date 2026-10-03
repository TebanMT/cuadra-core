// Tinta Access — Arduino Nano actuator proof
// Protocol: tinta-access-serial/1 @ 115200 8N1
//
// Host -> PING <command-id>
// Nano -> ACK <command-id> READY
//
// Host -> PULSE <command-id> <duration-ms>
// Nano -> ACK <command-id> STARTED
// Nano -> ACK <command-id> COMPLETED

#include <Arduino.h>

constexpr uint8_t ACTUATOR_PIN = LED_BUILTIN;
constexpr uint8_t ACTIVE_LEVEL = HIGH;
constexpr uint8_t INACTIVE_LEVEL = LOW;
constexpr unsigned long MIN_PULSE_MS = 100;
constexpr unsigned long MAX_PULSE_MS = 5000;
constexpr size_t COMMAND_ID_SIZE = 37;  // UUID string + null terminator.
constexpr size_t LINE_SIZE = 96;

char lineBuffer[LINE_SIZE];
size_t lineLength = 0;

bool pulseActive = false;
unsigned long pulseEndsAt = 0;
char activeCommand[COMMAND_ID_SIZE] = {0};
char lastCompletedCommand[COMMAND_ID_SIZE] = {0};

void copyCommandID(char *destination, const char *source) {
  strncpy(destination, source, COMMAND_ID_SIZE - 1);
  destination[COMMAND_ID_SIZE - 1] = '\0';
}

void emit(const char *kind, const char *commandID, const char *state) {
  Serial.print(kind);
  Serial.print(' ');
  Serial.print(commandID);
  Serial.print(' ');
  Serial.println(state);
}

void completePulse() {
  digitalWrite(ACTUATOR_PIN, INACTIVE_LEVEL);
  pulseActive = false;
  copyCommandID(lastCompletedCommand, activeCommand);
  activeCommand[0] = '\0';
  emit("ACK", lastCompletedCommand, "COMPLETED");
}

void handlePing(char *commandID) {
  if (commandID == nullptr || commandID[0] == '\0') {
    emit("ERR", "unknown", "MISSING_ID");
    return;
  }
  emit("ACK", commandID, "READY");
}

void handlePulse(char *commandID, char *durationText) {
  if (commandID == nullptr || durationText == nullptr) {
    emit("ERR", commandID == nullptr ? "unknown" : commandID, "INVALID_COMMAND");
    return;
  }

  // Repeating the last ID never pulses the output twice while this firmware
  // session remains alive. This protects host retries after a lost ACK.
  if (pulseActive && strcmp(commandID, activeCommand) == 0) {
    emit("ACK", commandID, "STARTED");
    return;
  }
  if (!pulseActive && strcmp(commandID, lastCompletedCommand) == 0) {
    emit("ACK", commandID, "COMPLETED");
    return;
  }
  if (pulseActive) {
    emit("ERR", commandID, "BUSY");
    return;
  }

  char *end = nullptr;
  unsigned long duration = strtoul(durationText, &end, 10);
  if (end == durationText || *end != '\0' || duration < MIN_PULSE_MS || duration > MAX_PULSE_MS) {
    emit("ERR", commandID, "INVALID_DURATION");
    return;
  }

  copyCommandID(activeCommand, commandID);
  digitalWrite(ACTUATOR_PIN, ACTIVE_LEVEL);
  pulseActive = true;
  pulseEndsAt = millis() + duration;
  emit("ACK", activeCommand, "STARTED");
}

void handleLine(char *line) {
  char *verb = strtok(line, " ");
  char *commandID = strtok(nullptr, " ");
  if (verb == nullptr) {
    return;
  }
  if (strcmp(verb, "PING") == 0) {
    handlePing(commandID);
    return;
  }
  if (strcmp(verb, "PULSE") == 0) {
    handlePulse(commandID, strtok(nullptr, " "));
    return;
  }
  emit("ERR", commandID == nullptr ? "unknown" : commandID, "UNKNOWN_COMMAND");
}

void readSerial() {
  while (Serial.available() > 0) {
    char value = static_cast<char>(Serial.read());
    if (value == '\r') {
      continue;
    }
    if (value == '\n') {
      lineBuffer[lineLength] = '\0';
      handleLine(lineBuffer);
      lineLength = 0;
      continue;
    }
    if (lineLength < LINE_SIZE - 1) {
      lineBuffer[lineLength++] = value;
    } else {
      lineLength = 0;
      emit("ERR", "unknown", "LINE_TOO_LONG");
    }
  }
}

void setup() {
  pinMode(ACTUATOR_PIN, OUTPUT);
  digitalWrite(ACTUATOR_PIN, INACTIVE_LEVEL);
  Serial.begin(115200);
  Serial.println("READY tinta-access-serial/1");
}

void loop() {
  // Signed subtraction keeps the comparison correct across millis() wrap.
  if (pulseActive && static_cast<long>(millis() - pulseEndsAt) >= 0) {
    completePulse();
  }
  readSerial();
}
