# ClassSend 2

Σύστημα διαχείρισης τάξης για Windows. Δάσκαλος + μαθητές, chat, παρακολούθηση οθονών, εντολές συστήματος.

## Τρέχουσα έκδοση

**v0.0.3** — πρώτη σταθερή release της 0.0.3 σειράς.

### Τι περιλαμβάνει

- **Παρακολούθηση οθονών**: grid με thumbnails, click σε cell για focus mode (υψηλή ανάλυση).
- **Πλήρης ανάλυση capture**: ο agent είναι DPI-aware και πιάνει ολόκληρη την επιφάνεια εργασίας ακόμα και σε οθόνες με Windows scaling > 100%.
- **Aspect-preserving display**: η οθόνη του μαθητή φαίνεται ολόκληρη χωρίς παραμόρφωση. Τα letterbox κενά γεμίζουν με το χρώμα του cell.
- **Layout**: padded cells, εικόνα στην κορυφή, hostname κάτω από την εικόνα.
- **Σταθερότητα pipe**: overlapped I/O ανάμεσα σε teacher και monitoring.exe με πραγματικά timeouts. Αντικαθιστά το παλιό σύγχρονο WriteFile που μπορούσε να «κολλήσει» επ' αόριστον.
- **Σταθερότητα δικτύου**: 10 s write deadline στις TCP εγγραφές — αποτρέπει το «κόλλημα» όταν μαθητής έχει σπάσει τη σύνδεση χωρίς να το ξέρει το σύστημα.
- **Cached back buffer στο monitoring**: το memory DC δημιουργείται μία φορά, όχι σε κάθε WM_PAINT — εξαφανίζει το flickering κατά το resize.
- **Diagnostic logging**: κάθε βήμα στη διαδρομή screenshot έχει timing log. Αν κάτι πάει στραβά, η αιτία φαίνεται με μια ματιά στα logs.
- **Bug reporting one-liner**: `--bug` ή `--report` φτιάχνει ένα zip με όλα τα σχετικά logs.

## Εντολές TUI

- `--about` / `--ver` / `--version` — εμφανίζει αυτό το αρχείο και την ενεργή έκδοση
- `--bug` / `--report` — δημιουργεί zip με logs στον φάκελο Downloads για αναφορά σφάλματος
- `--coffee` — διάλειμμα ☕
- `--matrix` — easter egg
- `--t lock/unlock/mute/close/launch/focus/shutdown/tvon/tvoff/shot` — εντολές συστήματος προς μαθητές
- `--set nickname <name>` — ορίζει όνομα
- `--set list import/export` — μαύρες/άσπρες λίστες
- `--clr @s` — καθαρίζει μηνύματα συστήματος

## Αναφορά σφάλματος

Αν κάτι δεν δουλεύει:

1. Πληκτρολόγησε `--bug` στο TUI.
2. Θα φτιαχτεί ένα zip στο `Downloads/classsend-bugreport-<timestamp>.zip` με όλα τα logs.
3. Στείλε το zip στο: **kalotrapezis@gmail.com** με σύντομη περιγραφή του προβλήματος.

Τα logs δεν περιέχουν προσωπικά δεδομένα — μόνο τεχνικές πληροφορίες για τη ροή των μηνυμάτων.

## CLI flags

- `teacher.exe --version` / `--ver` — εμφανίζει την έκδοση και κλείνει
- `classsend-agent.exe --version` — όμοιο για τον agent

## Επικοινωνία

kalotrapezis@gmail.com
