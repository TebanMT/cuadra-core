// tinta-bio — motor biométrico de Tinta (proceso hijo del sidecar Go).
//
// Responsabilidades:
//   1. Captura continua del lector U.are.U (DPUruNet, event-driven).
//   2. Extracción de FMDs (FingerJet, formato ANSI 378) por cada dedazo.
//   3. Enrolamiento (N FMDs de pre-enroll → FMD de enrollment).
//   4. Identificación 1:N contra una galería cacheada en memoria.
//
// Protocolo: NDJSON por stdin (comandos) / stdout (eventos + respuestas).
// Ver PROTOCOL.md. stderr es sólo logging humano (el sidecar lo re-loguea).
//
// Decisiones:
//   - Los FMD viajan y se almacenan como el XML de Fmd.SerializeXml en
//     base64 — opacos para el sidecar (round-trip garantizado por el SDK;
//     adentro el formato es ANSI 378, portable si algún día migramos).
//   - La galería vive aquí (comando `gallery`): mandar 500+ candidatos en
//     cada identify por stdio sería ~MBs por dedazo. El sidecar la
//     re-manda completa cuando cambia (enroll/baja) con un epoch.
//   - Un solo lector por proceso (los gyms tienen uno). Si hay varios,
//     TINTA_BIO_READER=<serial> elige; default: el primero.
//   - Re-arme defensivo de CaptureAsync tras cada evento: en algunas
//     versiones del SDK el arm es one-shot y en otras es continuo;
//     re-armar cuando ya está armado devuelve código de error inofensivo
//     que ignoramos. Preferimos un warning benigno a un lector sordo.
//
// EULA HID: este binario incorpora el runtime redistribuible de
// DigitalPersona. (c) HID Global — componentes FingerJet/DPUruNet
// redistribuidos bajo el EULA del DigitalPersona Biometric SDK.

using System.Runtime.InteropServices;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using DPUruNet;

namespace TintaBio;

internal static class Program
{
    // DPFJ_PROBABILITY_ONE del SDK — la escala del threshold de Identify.
    // threshold = PROBABILITY_ONE / farDivisor ⇒ FAR objetivo de 1/farDivisor.
    private const int ProbabilityOne = 0x7fffffff;
    private const int DefaultFarDivisor = 100_000;

    private static readonly JsonSerializerOptions Json = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull,
    };

    // Toda escritura a stdout pasa por este lock — eventos de captura
    // (thread del SDK) y respuestas de comandos (thread de stdin) no deben
    // intercalar bytes de una línea.
    private static readonly object StdoutLock = new();

    // FingerJet no documenta thread-safety — serializamos todas las
    // llamadas al engine (extract/enroll/identify) por este lock.
    private static readonly object EngineLock = new();

    // Galería 1:N: ref (uuid del template en el sidecar) → Fmd.
    // La reemplaza entera el comando `gallery`.
    private static List<(string Ref, Fmd Fmd)> _gallery = new();
    private static string _galleryEpoch = "";

    private static Reader? _reader;
    private static readonly CancellationTokenSource Shutdown = new();

    private static int Main(string[] args)
    {
        Console.OutputEncoding = Encoding.UTF8;
        InstallNativeResolver();
        Log($"tinta-bio start pid={Environment.ProcessId}");

        if (args.Contains("--list"))
        {
            // ReaderCollection implementa IEnumerable NO genérico — el
            // foreach tipado hace el cast por elemento (igual que el sample).
            foreach (Reader r in ReaderCollection.GetReaders())
                Console.WriteLine($"{r.Description.SerialNumber}\t{r.Description.Name}");
            return 0;
        }

        // Loop del lector en su propio thread (LongRunning: el Capture
        // bloqueante lo ocupa permanentemente); stdin es el hilo principal.
        // stdin EOF = el sidecar murió o nos pidió salir → shutdown limpio.
        var readerLoop = Task.Factory.StartNew(
            ReaderLoop, CancellationToken.None,
            TaskCreationOptions.LongRunning, TaskScheduler.Default);
        try
        {
            string? line;
            while ((line = Console.In.ReadLine()) != null)
            {
                if (string.IsNullOrWhiteSpace(line)) continue;
                HandleCommand(line);
            }
        }
        catch (Exception ex)
        {
            Log($"stdin loop error: {ex.Message}");
        }

        Log("stdin EOF — shutting down");
        Shutdown.Cancel();
        try { _reader?.CancelCapture(); } catch { /* best effort */ }
        try { _reader?.Dispose(); } catch { /* best effort */ }
        // Darle al loop una oportunidad de salir ordenado, sin colgar el exit.
        readerLoop.Wait(TimeSpan.FromSeconds(2));
        return 0;
    }

    // ── Lector: enumerar → abrir → loop de captura BLOQUEANTE ──────────────
    //
    // Se usa Reader.Capture (síncrono, con timeout) en un thread dedicado,
    // NO CaptureAsync/On_Captured: la entrega de eventos del SDK está
    // pensada para apps WinForms con message pump — en este proceso de
    // consola los On_Captured nunca dispararon (visto en hardware real:
    // reader connected, dedazo, silencio). El bloqueante es determinista:
    // capture → resultado → procesar → volver a capturar. El timeout de
    // cada vuelta funge además de checkpoint de vida del dispositivo (un
    // lector desconectado hace fallar el Capture → reabrimos con backoff).

    private const int CaptureTimeoutMs = 5000;

    private static void ReaderLoop()
    {
        var backoffMs = 1000;
        while (!Shutdown.IsCancellationRequested)
        {
            Reader? reader = null;
            try
            {
                var wanted = Environment.GetEnvironmentVariable("TINTA_BIO_READER");
                // Cast explícito: ReaderCollection sólo expone IEnumerable
                // no genérico (verificado contra el XML doc del assembly).
                reader = ReaderCollection.GetReaders().Cast<Reader>().FirstOrDefault(r =>
                    string.IsNullOrEmpty(wanted) ||
                    string.Equals(r.Description.SerialNumber, wanted, StringComparison.OrdinalIgnoreCase));

                if (reader == null)
                {
                    Emit(new Evt { Event = "reader", State = "disconnected", Code = "no_device" });
                    Sleep(backoffMs);
                    backoffMs = Math.Min(backoffMs * 2, 8000);
                    continue;
                }

                var rc = reader.Open(Constants.CapturePriority.DP_PRIORITY_COOPERATIVE);
                if (rc != Constants.ResultCode.DP_SUCCESS)
                {
                    Emit(new Evt { Event = "reader", State = "disconnected", Code = rc.ToString() });
                    reader.Dispose();
                    Sleep(backoffMs);
                    backoffMs = Math.Min(backoffMs * 2, 8000);
                    continue;
                }

                _reader = reader;
                backoffMs = 1000;
                var resolution = reader.Capabilities.Resolutions[0];

                Emit(new Evt
                {
                    Event = "reader",
                    State = "connected",
                    Name = reader.Description.Name,
                    Serial = reader.Description.SerialNumber,
                });

                while (!Shutdown.IsCancellationRequested)
                {
                    var capture = reader.Capture(
                        Constants.Formats.Fid.ANSI,
                        Constants.CaptureProcessing.DP_IMG_PROC_DEFAULT,
                        resolution,
                        CaptureTimeoutMs);

                    if (Shutdown.IsCancellationRequested) break;
                    if (capture == null) continue;

                    if (capture.ResultCode != Constants.ResultCode.DP_SUCCESS)
                    {
                        // El Capture bloqueante reporta la desconexión del
                        // device como fallo — salir a re-enumerar/reabrir.
                        Log($"capture rc={capture.ResultCode} — reopening");
                        break;
                    }

                    HandleSample(capture);
                }
            }
            catch (Exception ex)
            {
                Log($"reader loop: {ex.Message}");
                Emit(new Evt { Event = "reader", State = "disconnected", Code = "exception", Detail = ex.Message });
                Sleep(backoffMs);
                backoffMs = Math.Min(backoffMs * 2, 8000);
            }
            finally
            {
                try { reader?.CancelCapture(); } catch { }
                try { reader?.Dispose(); } catch { }
                if (_reader != null && !Shutdown.IsCancellationRequested)
                    Emit(new Evt { Event = "reader", State = "disconnected", Code = "reopening" });
                _reader = null;
            }
        }
    }

    private static void HandleSample(CaptureResult capture)
    {
        try
        {
            var quality = capture.Quality.ToString();

            // Timeout de la vuelta (sin dedo) y cancelaciones no son
            // dedazos: seguir capturando en silencio — emitirlos inundaría
            // stdout con un evento cada CaptureTimeoutMs.
            if (quality.Contains("TIMED_OUT") || quality.Contains("CANCELED"))
                return;

            if (capture.Data == null)
            {
                // Hubo dedo pero la captura no sirvió (calidad) — feedback
                // para el FE ("vuelve a apoyar") sin registrar nada.
                Emit(new Evt { Event = "sample_rejected", Code = "no_data", Quality = quality });
                return;
            }

            Fmd fmd;
            lock (EngineLock)
            {
                var extraction = FeatureExtraction.CreateFmdFromFid(capture.Data, Constants.Formats.Fmd.ANSI);
                if (extraction.ResultCode != Constants.ResultCode.DP_SUCCESS)
                {
                    Emit(new Evt { Event = "sample_rejected", Code = extraction.ResultCode.ToString(), Quality = quality });
                    return;
                }
                fmd = extraction.Data;
            }

            Emit(new Evt
            {
                Event = "sample",
                Fmd = PackFmd(fmd),
                Quality = quality,
                Score = capture.Score,
            });
        }
        catch (Exception ex)
        {
            Emit(new Evt { Event = "error", Code = "capture_handler", Detail = ex.Message });
        }
    }

    // ── Comandos ────────────────────────────────────────────────────────────

    private static void HandleCommand(string line)
    {
        Cmd? cmd = null;
        try
        {
            cmd = JsonSerializer.Deserialize<Cmd>(line, Json);
            if (cmd?.Command == null)
            {
                Emit(new Evt { Event = "result", Ok = false, Code = "bad_command" });
                return;
            }

            switch (cmd.Command)
            {
                case "ping":
                    Emit(new Evt
                    {
                        Event = "result", Id = cmd.Id, Ok = true,
                        State = _reader != null ? "connected" : "disconnected",
                        GalleryEpoch = _galleryEpoch,
                        GallerySize = _gallery.Count,
                    });
                    break;

                case "gallery":
                {
                    // Reemplazo TOTAL de la galería. El sidecar es la fuente
                    // de verdad; acá sólo cacheamos deserializado.
                    var next = new List<(string, Fmd)>(cmd.Candidates?.Count ?? 0);
                    foreach (var c in cmd.Candidates ?? new())
                        next.Add((c.Ref!, UnpackFmd(c.Fmd!)));
                    lock (EngineLock)
                    {
                        _gallery = next;
                        _galleryEpoch = cmd.Epoch ?? "";
                    }
                    Emit(new Evt { Event = "result", Id = cmd.Id, Ok = true, GallerySize = next.Count });
                    break;
                }

                case "identify":
                {
                    var probe = UnpackFmd(cmd.Probe!);
                    var divisor = cmd.FarDivisor ?? DefaultFarDivisor;
                    var max = cmd.Max ?? 1;
                    List<(string Ref, Fmd Fmd)> gallery;
                    IdentifyResult result;
                    lock (EngineLock)
                    {
                        gallery = _gallery;
                        result = Comparison.Identify(
                            probe, 0,
                            gallery.Select(g => g.Fmd),
                            ProbabilityOne / divisor,
                            max);
                    }
                    if (result.ResultCode != Constants.ResultCode.DP_SUCCESS)
                    {
                        Emit(new Evt { Event = "result", Id = cmd.Id, Ok = false, Code = result.ResultCode.ToString() });
                        break;
                    }
                    // Indexes: int[][] — [índice de candidato, índice de vista].
                    var matches = (result.Indexes ?? Array.Empty<int[]>())
                        .Where(ix => ix.Length > 0 && ix[0] >= 0 && ix[0] < gallery.Count)
                        .Select(ix => gallery[ix[0]].Ref)
                        .ToArray();
                    Emit(new Evt
                    {
                        Event = "result", Id = cmd.Id, Ok = true,
                        Matches = matches, GalleryEpoch = _galleryEpoch,
                    });
                    break;
                }

                case "enroll":
                {
                    // N FMDs de pre-enroll (capturas sueltas del mismo dedo)
                    // → un FMD de enrollment. El SDK exige suficientes
                    // capturas consistentes; si no alcanza, devuelve error
                    // y el FE pide otro dedazo.
                    var pre = (cmd.Fmds ?? new()).Select(UnpackFmd).ToList();
                    DataResult<Fmd> enrollment;
                    lock (EngineLock)
                    {
                        enrollment = Enrollment.CreateEnrollmentFmd(Constants.Formats.Fmd.ANSI, pre);
                    }
                    if (enrollment.ResultCode != Constants.ResultCode.DP_SUCCESS)
                    {
                        Emit(new Evt { Event = "result", Id = cmd.Id, Ok = false, Code = enrollment.ResultCode.ToString() });
                        break;
                    }
                    Emit(new Evt { Event = "result", Id = cmd.Id, Ok = true, Fmd = PackFmd(enrollment.Data) });
                    break;
                }

                case "compare":
                {
                    // 1:1 — para el check de colisión al enrolar (¿este dedo
                    // ya está registrado a nombre de otro socio?) el sidecar
                    // puede usar identify; compare queda para diagnósticos.
                    var a = UnpackFmd(cmd.Probe!);
                    var b = UnpackFmd(cmd.Fmds![0]);
                    CompareResult cr;
                    lock (EngineLock) { cr = Comparison.Compare(a, 0, b, 0); }
                    Emit(new Evt
                    {
                        Event = "result", Id = cmd.Id,
                        Ok = cr.ResultCode == Constants.ResultCode.DP_SUCCESS,
                        Score = cr.Score,
                        Code = cr.ResultCode.ToString(),
                    });
                    break;
                }

                default:
                    Emit(new Evt { Event = "result", Id = cmd.Id, Ok = false, Code = "unknown_command" });
                    break;
            }
        }
        catch (Exception ex)
        {
            Emit(new Evt { Event = "result", Id = cmd?.Id, Ok = false, Code = "exception", Detail = ex.Message });
        }
    }

    // ── FMD packing: XML del SDK ↔ base64 opaco ─────────────────────────────

    private static string PackFmd(Fmd fmd) =>
        Convert.ToBase64String(Encoding.UTF8.GetBytes(Fmd.SerializeXml(fmd)));

    private static Fmd UnpackFmd(string packed) =>
        Fmd.DeserializeXml(Encoding.UTF8.GetString(Convert.FromBase64String(packed)));

    // ── resolución de DLLs nativas del SDK ──────────────────────────────────
    //
    // dpfpdd.dll NO es autosuficiente: su enumeración de dispositivos
    // depende de la capa de soporte que instala el RTE (dpusbada, dpdevctl,
    // dpdevdat, módulos dpd*/dpi*, blobs de firmware…). Copiar sólo las
    // DLLs de Lib/x64 junto al exe le hace SOMBRA al runtime completo del
    // sistema y enumera cero lectores (visto en campo: no_device con el
    // sample funcionando en la misma máquina). Regla: NUNCA shippear
    // nativas del SDK junto al exe. Este resolver sólo ayuda a ENCONTRAR
    // el runtime instalado cuando el loader no lo tiene en su búsqueda
    // default (PATH sin registrar, instalaciones custom): resuelve los
    // DllImport de DPUruNet hacia el directorio de instalación del RTE,
    // para que dpfpdd cargue DESDE su casa, con toda su capa alrededor.
    private static void InstallNativeResolver()
    {
        NativeLibrary.SetDllImportResolver(typeof(Reader).Assembly, (name, assembly, searchPath) =>
        {
            // Primero la búsqueda default (PATH, System32, app dir) — si el
            // sistema ya resuelve, no interferimos.
            if (NativeLibrary.TryLoad(name, assembly, searchPath, out var handle))
                return handle;

            var file = name.EndsWith(".dll", StringComparison.OrdinalIgnoreCase) ? name : name + ".dll";
            foreach (var dir in NativeDirCandidates())
            {
                var candidate = Path.Combine(dir, file);
                if (File.Exists(candidate) && NativeLibrary.TryLoad(candidate, out handle))
                {
                    Log($"native {file} <- {dir}");
                    return handle;
                }
            }
            return IntPtr.Zero; // que el error original suba con claridad
        });
    }

    private static IEnumerable<string> NativeDirCandidates()
    {
        // Override explícito para diagnóstico/instalaciones raras.
        var env = Environment.GetEnvironmentVariable("TINTA_BIO_NATIVE_DIR");
        if (!string.IsNullOrEmpty(env)) yield return env;

        // Rutas de instalación conocidas del RTE / SDK (x64).
        var pf = Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles);
        yield return Path.Combine(pf, "DigitalPersona", "Bin");
        yield return Path.Combine(pf, "DigitalPersona", "U.are.U SDK", "Windows", "Lib", "x64");
        yield return Path.Combine(pf, "HID Global", "DigitalPersona", "Bin");
    }

    // ── plumbing ────────────────────────────────────────────────────────────

    private static void Emit(Evt evt)
    {
        var line = JsonSerializer.Serialize(evt, Json);
        lock (StdoutLock)
        {
            Console.Out.WriteLine(line);
            Console.Out.Flush();
        }
    }

    private static void Log(string msg) => Console.Error.WriteLine($"[tinta-bio] {msg}");

    // Sleep cancelable por shutdown (el WaitHandle despierta al instante
    // cuando Shutdown.Cancel() dispara — sin esperar el timeout completo).
    private static void Sleep(int ms) => Shutdown.Token.WaitHandle.WaitOne(ms);

    // ── wire types ──────────────────────────────────────────────────────────

    internal sealed class Cmd
    {
        [JsonPropertyName("cmd")] public string? Command { get; set; }
        public string? Id { get; set; }
        public string? Probe { get; set; }
        public List<string>? Fmds { get; set; }
        public List<Candidate>? Candidates { get; set; }
        public string? Epoch { get; set; }
        public int? FarDivisor { get; set; }
        public int? Max { get; set; }
    }

    internal sealed class Candidate
    {
        public string? Ref { get; set; }
        public string? Fmd { get; set; }
    }

    internal sealed class Evt
    {
        public string? Event { get; set; }
        public string? Id { get; set; }
        public bool? Ok { get; set; }
        public string? State { get; set; }
        public string? Name { get; set; }
        public string? Serial { get; set; }
        public string? Code { get; set; }
        public string? Detail { get; set; }
        public string? Fmd { get; set; }
        public string? Quality { get; set; }
        public int? Score { get; set; }
        public string[]? Matches { get; set; }
        public string? GalleryEpoch { get; set; }
        public int? GallerySize { get; set; }
    }
}
