# Representación de las pruebas de dispositivo

S00 permite timestamps UTC con segundos o tres decimales. La emisión pública y los payloads firmados de prueba reciente/reanudación usan **siempre tres decimales**, incluido `.000Z` al segundo exacto. El móvil puede usar `Date.toISOString()`; al parsear una entrada semánticamente equivalente sin milisegundos, el backend la normaliza al mismo payload. No se admiten fracciones menores a un milisegundo.

Corrección previa a integrar el cliente: el helper inicial omitía `.000` si la fracción era cero, mientras el DTO ya emitía `.000Z`; eso invalidaba una firma correcta sobre el valor visible. Los vectores independientes de `TestWholeSecondProofsMatchPublicMillisecondsAndNormalizeEquivalentInput` comprueban los bytes y ambas firmas con entradas `Z`/`.000Z`. El esquema RFC3339 del contrato no cambia. La firma cubre las representaciones canónicas definidas por S00, sin formatos alternativos ni fallback de firma.
