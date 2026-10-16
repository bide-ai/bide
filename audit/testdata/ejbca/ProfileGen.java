// Writes the certificate profile "Approver" in EJBCA's profile XML format, built with EJBCA's own
// classes (run inside the container: java -cp "<ejbca-ejb-cli/lib>/*" ProfileGen.java <certprofile.xml> <endentityprofile.xml> <certprofile id>).
// It is the approver template of the approver identity design: the bank EKU, the approver policy
// OID, digitalSignature only, a 24 hour validity, and the CA's default ten minute notBefore offset.
import java.beans.XMLEncoder;
import java.io.FileOutputStream;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.cesecore.certificates.certificate.CertificateConstants;
import com.keyfactor.util.certificate.DnComponents;
import org.ejbca.core.model.ra.raadmin.EndEntityProfile;
import org.cesecore.certificates.certificateprofile.CertificatePolicy;
import org.cesecore.certificates.certificateprofile.CertificateProfile;

public class ProfileGen {
  @SuppressWarnings({"unchecked", "rawtypes"})
  public static void main(String[] a) throws Exception {
    CertificateProfile p = new CertificateProfile(CertificateConstants.CERTTYPE_ENDENTITY);
    p.setEncodedValidity("1d");
    p.setUseCertificateValidityOffset(true);
    p.setCertificateValidityOffset("-10m");
    p.setUseExtendedKeyUsage(true);
    p.setExtendedKeyUsageOids(new ArrayList<>(List.of("1.3.6.1.4.1.99999.1.1")));
    p.setUseCertificatePolicies(true);
    p.setCertificatePolicies(new ArrayList<>(List.of(new CertificatePolicy("1.3.6.1.4.1.99999.2.1", null, null))));
    p.setUseKeyUsage(true);
    boolean[] ku = new boolean[9];
    ku[0] = true; // digitalSignature
    p.setKeyUsage(ku);
    p.setKeyUsageCritical(true);
    LinkedHashMap<Object, Object> m = new LinkedHashMap<>((Map) p.saveData());
    try (XMLEncoder e = new XMLEncoder(new FileOutputStream(a[0]))) {
      e.writeObject(m);
    }
    // The end entity profile "Approver": the certificate profile above, any CA, a URI subject
    // alternative name.
    EndEntityProfile ep = new EndEntityProfile(true);
    ep.addField(DnComponents.UNIFORMRESOURCEID);
    ep.setAvailableCertificateProfileIds(new ArrayList<>(List.of(Integer.parseInt(a[2]))));
    ep.setDefaultCertificateProfile(Integer.parseInt(a[2]));
    LinkedHashMap<Object, Object> em = new LinkedHashMap<>((Map) ep.saveData());
    try (XMLEncoder e = new XMLEncoder(new FileOutputStream(a[1]))) {
      e.writeObject(em);
    }
  }
}
