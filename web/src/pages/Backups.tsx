import AttachmentStoragePanel from "../components/AttachmentStoragePanel";
import BackupsPanel from "../components/BackupsPanel";

export default function Backups() {
  return (
    <div className="page">
      <header className="page-header">
        <h1>Backups</h1>
      </header>

      <BackupsPanel />
      <AttachmentStoragePanel />
    </div>
  );
}
